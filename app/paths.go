package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/worktree"
)

// Paths 是一次运行用到的三个目录：工作目录、目标仓库主 checkout、
// worktree 父目录。
type Paths struct {
	WorkDir, RepoDir, TreeDir string
}

// PathsPreview 是三个目录的解析结果：能解析的都填好，失败的那一项留
// 空并在对应的 Err 里说明原因。它只读本地文件系统与 git，不碰 forge
// 网络，也不写任何配置——因此界面可以在运行前反复预览。
type PathsPreview struct {
	Paths Paths
	// RepoErr 非空表示仓库目录没能解析（或不是仓库顶层）。
	RepoErr error
	// RepoCandidates 是自动搜索在工作目录附近发现的多个候选仓库根（已排序），
	// 仅在多仓库歧义时填充，供 GUI 直接列出来给用户选择。
	RepoCandidates []string
	// TreeErr 非空表示 worktree 目录没能解析。
	TreeErr error
	// TreeCandidates 是仓库既有 worktree 分散在多个父目录时的候选列表（已排序），
	// 供 GUI 直接列出来给用户选择。
	TreeCandidates []string
}

// Ready 报告三个目录是否都已解析。
func (p PathsPreview) Ready() bool {
	return p.Paths.WorkDir != "" && p.Paths.RepoDir != "" && p.Paths.TreeDir != "" && p.RepoErr == nil && p.TreeErr == nil
}

// PreviewPaths 用与 app.Run 完全相同的规则预览三个目录：
// resolveWorkDir → config.Load → resolveRepoDir（并校验仓库顶层）→
// resolveTreeDir。它不发 forge 网络请求；配置读取遵循 config.Load 的
// 既有行为（若默认配置层不存在，可能播种默认配置文件），但不会把界面
// 中用户输入的目录写回配置。返回的错误只表示工作目录或配置本身不可用，
// 此时界面按普通失败提示；仓库与 worktree 目录不可用分别记在
// RepoErr/TreeErr，已经解析出的目录仍然保留，界面据此只补填缺失的那一项。
func PreviewPaths(ctx context.Context, opts Options) (PathsPreview, error) {
	workDir, err := resolveWorkDir(opts.WorkDir)
	if err != nil {
		return PathsPreview{}, err
	}
	cfg, err := config.Load(workDir, opts.ConfigPath)
	if err != nil {
		return PathsPreview{}, err
	}
	out := PathsPreview{Paths: Paths{WorkDir: workDir}}

	repoDir, repoErr := resolveRepoDir(ctx, opts.RepoDir, cfg.RepoDir, workDir)
	switch {
	case repoErr != nil:
		var multi *worktree.MultipleReposError
		if errors.As(repoErr, &multi) {
			out.RepoCandidates = append([]string(nil), multi.Repos...)
		}
		// 自动查找失败也是仓库错误：界面据此让用户填写或挑选仓库目录。
		if !IsRepositoryError(repoErr) {
			repoErr = repoError(opts.RepoDir, repoErr)
		}
	default:
		// GUI 的显式仓库目录必须本身就是仓库顶层：git 在子目录里也
		// 能跑，会把父仓库的 origin 认成本项目的。
		repoErr = verifyRepoRoot(ctx, repoDir)
	}
	if repoErr != nil {
		out.RepoErr = repoErr
		return out, nil
	}
	out.Paths.RepoDir = repoDir

	// 用户显式切换到与配置中 repo_dir 不同的仓库时，不能把旧仓库的 tree_dir
	// 张冠李戴过来，必须按新仓库重新推断 worktree 目录。
	cfgTreeDir := cfg.TreeDir
	if strings.TrimSpace(opts.RepoDir) != "" && strings.TrimSpace(cfg.RepoDir) != "" && !sameDirPath(repoDir, cfg.RepoDir) {
		cfgTreeDir = ""
	}
	treeDir, treeErr := resolveTreeDir(ctx, opts.TreeDir, cfgTreeDir, repoDir)
	if treeErr != nil {
		var multi *worktree.MultipleTreeDirsError
		if errors.As(treeErr, &multi) {
			out.TreeCandidates = append([]string(nil), multi.Dirs...)
		}
		out.TreeErr = errors.Join(fmt.Errorf("resolve tree-dir: %w", treeErr))
		return out, nil
	}
	out.Paths.TreeDir = treeDir
	return out, nil
}

// sameDirPath 比较两个目录路径是否指向同一位置（先 Abs+Clean，存在时再解析符号链接）。
func sameDirPath(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	absA, absB = filepath.Clean(absA), filepath.Clean(absB)
	if absA == absB {
		return true
	}
	if realA, err := filepath.EvalSymlinks(absA); err == nil {
		absA = realA
	}
	if realB, err := filepath.EvalSymlinks(absB); err == nil {
		absB = realB
	}
	return absA == absB
}
