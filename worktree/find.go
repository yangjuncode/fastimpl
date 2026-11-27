package worktree

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// MaxSearchDepth 是向上/向下搜索仓库根的最大层数。
const MaxSearchDepth = 8

// MultipleReposError 表示在搜索范围内找到了多个 Git 仓库根，
// 需要调用方从中挑选一个。Repos 已按字典序排序，均为绝对路径。
type MultipleReposError struct {
	Start string
	Repos []string
}

func (e *MultipleReposError) Error() string {
	return fmt.Sprintf("multiple git repositories found near %s: %s; use -repo-dir to pick one",
		e.Start, strings.Join(e.Repos, ", "))
}

// MultipleTreeDirsError 表示仓库既有 worktree 分散在多个父目录下，
// 需要调用方从中挑选一个。Dirs 已按字典序排序，均为绝对路径。
type MultipleTreeDirsError struct {
	Dirs []string
}

func (e *MultipleTreeDirsError) Error() string {
	return fmt.Sprintf("existing worktrees have no uniform parent directory: %s; use -tree-dir to pick one",
		strings.Join(e.Dirs, ", "))
}

// FindRoot 从 start 出发定位 Git 仓库根目录：
//  1. start 本身是仓库根 → 直接返回（默认就是当前目录）；
//  2. 否则向上（父目录链）与向下（子目录树）各搜至多 MaxSearchDepth 级；
//  3. 恰命中一个仓库根 → 返回它；零个报未找到；多个报歧义，
//     提示用 -repo-dir 显式指定（并经 MultipleReposError 返回完整候选列表）。
//
// 仓库根的判定是该目录下存在 .git 目录；.git 文件的是 linked
// worktree/submodule，不当作可管理的主仓库根。向下搜索不进入
// 隐藏目录，也不进入已判定的仓库内部。
func FindRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", start, err)
	}
	abs = filepath.Clean(abs)
	if IsRepoRoot(abs) {
		return abs, nil
	}
	found := map[string]bool{}

	// 向上：沿父目录链至多 8 级。
	for dir, depth := abs, 0; depth < MaxSearchDepth; depth++ {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
		if IsRepoRoot(dir) {
			found[dir] = true
		}
	}

	// 向下：BFS 子目录树至多 8 级。
	type entry struct {
		dir   string
		depth int
	}
	queue := []entry{{abs, 0}}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e.depth >= MaxSearchDepth {
			continue
		}
		items, err := os.ReadDir(e.dir)
		if err != nil {
			continue // 权限不足等目录直接跳过
		}
		for _, it := range items {
			// IsDir 对 symlink 为 false，顺带排除了符号链接。
			if !it.IsDir() || strings.HasPrefix(it.Name(), ".") {
				continue
			}
			child := filepath.Join(e.dir, it.Name())
			if IsRepoRoot(child) {
				found[child] = true
				continue // 不进入仓库内部
			}
			queue = append(queue, entry{child, e.depth + 1})
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("no git repository found within %d levels up or down from %s; use -repo-dir", MaxSearchDepth, abs)
	case 1:
		return slices.Collect(maps.Keys(found))[0], nil
	default:
		return "", &MultipleReposError{
			Start: abs,
			Repos: slices.Sorted(maps.Keys(found)),
		}
	}
}

// IsRepoRoot 报告 dir 是否是仓库顶层：存在 .git 目录。
func IsRepoRoot(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

// ForgeForHost 按 origin 主机名推断 forge 类型：
// github.com 及其子域 → github；主机名含 gitlab → gitlab；
// 其他（自建实例等）→ 空串，由调用方按默认 forge（gitlab）处理。
// 推断结果可用配置 forge 显式覆盖。
func ForgeForHost(host string) string {
	h := strings.ToLower(host)
	if h == "github.com" || strings.HasSuffix(h, ".github.com") {
		return "github"
	}
	if strings.Contains(h, "gitlab") {
		return "gitlab"
	}
	return ""
}

// DefaultTreeDir 是没有可推断的既有 worktree 时的默认 worktree 父目录。
func DefaultTreeDir(repoDir string) string {
	return filepath.Join(repoDir, ".worktrees")
}

// ResolveTreeDir 解析 worktree 父目录：
//  1. explicit 非空（-tree-dir / 配置 tree_dir）→ 原样返回；
//  2. 仓库既有 worktree（git worktree list，排除主 checkout）的父目录
//     统一 → 用该统一目录；分散在多个目录 → 返回 MultipleTreeDirsError
//     列出各父目录，提示 -tree-dir；
//  3. 无既有 worktree → DefaultTreeDir(repoDir)。
func ResolveTreeDir(ctx context.Context, repoDir, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	r := &Repo{Root: repoDir}
	out, err := r.run(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	parents := map[string]bool{}
	root := filepath.Clean(repoDir)
	for line := range strings.Lines(out) {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		p := filepath.Clean(strings.TrimSpace(path))
		if p == root {
			continue // 排除主 checkout
		}
		parents[filepath.Dir(p)] = true
	}
	switch len(parents) {
	case 0:
		return DefaultTreeDir(repoDir), nil
	case 1:
		for parent := range parents {
			return parent, nil
		}
	}
	return "", &MultipleTreeDirsError{
		Dirs: slices.Sorted(maps.Keys(parents)),
	}
}
