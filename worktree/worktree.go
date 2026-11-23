// Package worktree 由 Go 代码直接管理 git worktree 与分支（不经 agent）：
// spec 的集成分支一个 worktree，每个子任务一个 issue-<iid> 分支 worktree。
package worktree

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Repo 是目标仓库主 checkout 与其 worktree 布局。
type Repo struct {
	// Root 是仓库顶层目录（主 checkout）。
	Root string
	// TreeDir 是所有 fastimpl worktree 的父目录。
	TreeDir string
	// Name 是仓库短名，作 hc 终端命名前缀（<name>-issue-<iid>），
	// 多项目并行时避免终端名碰撞。默认取 Root 目录名，
	// app 用 origin 项目路径的最后一段覆盖。
	Name string
}

// NewRepo 构造 Repo；root 必须是仓库顶层目录。
func NewRepo(root, treeDir string) *Repo {
	return &Repo{Root: root, TreeDir: treeDir, Name: filepath.Base(filepath.Clean(root))}
}

// BranchName 返回子任务的工作分支名：issue-<iid>。
func BranchName(iid int) string {
	return "issue-" + strconv.Itoa(iid)
}

// Dir 返回子任务 worktree 的目录。
func (r *Repo) Dir(iid int) string {
	return filepath.Join(r.TreeDir, BranchName(iid))
}

// IntegrationDir 返回集成分支 worktree 的目录。
func (r *Repo) IntegrationDir(spec int) string {
	return filepath.Join(r.TreeDir, "issue-"+strconv.Itoa(spec)+"-integration")
}

// run 在 dir 中执行 git，标准错误并入错误信息。
func (r *Repo) run(ctx context.Context, dir string, argv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// IntegrationBranch 返回 spec 的集成分支名：issue-<spec>-integration。
func IntegrationBranch(spec int) string {
	return "issue-" + strconv.Itoa(spec) + "-integration"
}

// BranchPrefix 返回 issue 分支的统一前缀：issue-<iid>。
// 规范允许 issue-<iid> 或 issue-<iid>-<描述>，匹配时都按此前缀处理。
func BranchPrefix(iid int) string {
	return "issue-" + strconv.Itoa(iid)
}

// issueName 报告 name 是否是 issue <iid> 的分支/目录名：
// 恰为 issue-<iid> 或以 issue-<iid>- 开头。
func issueName(name string, iid int) bool {
	prefix := BranchPrefix(iid)
	return name == prefix || strings.HasPrefix(name, prefix+"-")
}

// branchExists 报告本地分支是否存在。
func (r *Repo) branchExists(ctx context.Context, name string) bool {
	_, err := r.run(ctx, r.Root, "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// findIssueBranch 查找已存在的 issue-<iid>(-*)? 分支；
// 找到返回分支名，找不到返回空串。
func (r *Repo) findIssueBranch(ctx context.Context, iid int) string {
	out, err := r.run(ctx, r.Root, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return ""
	}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if issueName(name, iid) {
			return name
		}
	}
	return ""
}

// findIssueDir 在 TreeDir 下查找已存在的 issue-<iid>(-*)? worktree 目录。
func (r *Repo) findIssueDir(iid int) string {
	entries, err := os.ReadDir(r.TreeDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() && issueName(e.Name(), iid) {
			if _, statErr := os.Stat(filepath.Join(r.TreeDir, e.Name(), ".git")); statErr == nil {
				return filepath.Join(r.TreeDir, e.Name())
			}
		}
	}
	return ""
}

// baseName 确定集成分支的基点名（MR 目标）：显式 base >
// main > master > origin/HEAD 指向的分支 > "main" 兜底。
func (r *Repo) baseName(ctx context.Context, base string) string {
	if base != "" {
		return base
	}
	for _, name := range []string{"main", "master"} {
		if _, err := r.run(ctx, r.Root, "rev-parse", "--verify", "origin/"+name); err == nil {
			return name
		}
		if r.branchExists(ctx, name) {
			return name
		}
	}
	if out, err := r.run(ctx, r.Root, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(out); strings.HasPrefix(ref, "origin/") {
			return strings.TrimPrefix(ref, "origin/")
		}
	}
	return "main"
}

// baseRef 把基点名解析为可建分支的 ref：优先 origin/<base>。
func (r *Repo) baseRef(ctx context.Context, base string) string {
	if _, err := r.run(ctx, r.Root, "rev-parse", "--verify", "origin/"+base); err == nil {
		return "origin/" + base
	}
	return base
}

// EnsureIntegration 确保 spec 的集成分支与它的 worktree 存在，
// 返回分支名、worktree 目录与 MR 目标基点名。
// 既有 issue-<spec>(-*)? 分支/worktree 直接复用；没有则按
// main > master > origin/HEAD 顺序建 issue-<spec>-integration。
func (r *Repo) EnsureIntegration(ctx context.Context, spec int, base string) (branch, dir, baseName string, err error) {
	baseName = r.baseName(ctx, base)
	if err := os.MkdirAll(r.TreeDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create tree dir %s: %w", r.TreeDir, err)
	}
	// worktree 目录已存在：直接复用（分支名以目录名为准）。
	if existing := r.findIssueDir(spec); existing != "" {
		return filepath.Base(existing), existing, baseName, nil
	}
	branch = r.findIssueBranch(ctx, spec)
	if branch == "" {
		branch = IntegrationBranch(spec)
		if _, err := r.run(ctx, r.Root, "branch", branch, r.baseRef(ctx, baseName)); err != nil {
			return "", "", "", fmt.Errorf("create integration branch %s: %w", branch, err)
		}
	}
	dir = filepath.Join(r.TreeDir, branch)
	if _, err := r.run(ctx, r.Root, "worktree", "add", dir, branch); err != nil {
		return "", "", "", fmt.Errorf("add integration worktree %s: %w", dir, err)
	}
	return branch, dir, baseName, nil
}

// Create 为子任务从集成分支建 issue-<iid>(-*)? 分支与 worktree。
// 已有匹配的 worktree 目录直接复用；有匹配分支复用该分支；
// 都没有则从集成分支新建 issue-<iid>。返回目录与实际分支名。
func (r *Repo) Create(ctx context.Context, spec, iid int) (dir, branch string, err error) {
	if existing := r.findIssueDir(iid); existing != "" {
		return existing, filepath.Base(existing), nil
	}
	branch = r.findIssueBranch(ctx, iid)
	if branch == "" {
		branch = BranchName(iid)
	}
	dir = filepath.Join(r.TreeDir, branch)
	if _, statErr := os.Stat(dir); statErr == nil {
		return "", "", fmt.Errorf("task worktree %s already exists; remove it before retrying", dir)
	}
	if r.branchExists(ctx, branch) {
		if _, err := r.run(ctx, r.Root, "worktree", "add", dir, branch); err != nil {
			return "", "", fmt.Errorf("add worktree on existing branch %s: %w", branch, err)
		}
		return dir, branch, nil
	}
	if _, err := r.run(ctx, r.Root, "worktree", "add", dir, "-b", branch, IntegrationBranch(spec)); err != nil {
		return "", "", fmt.Errorf("add worktree %s on %s: %w", dir, branch, err)
	}
	return dir, branch, nil
}

// Remove 删除子任务的 worktree 与分支（任务集成并关闭后调用）：
// 删除 TreeDir 下全部匹配 issue-<iid>(-*)? 的目录与同名分支。
func (r *Repo) Remove(ctx context.Context, iid int) error {
	entries, err := os.ReadDir(r.TreeDir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() && issueName(e.Name(), iid) {
				dir := filepath.Join(r.TreeDir, e.Name())
				// worktree 内可能有未跟踪的 agent 产物，用 --force 删除。
				if _, err := r.run(ctx, r.Root, "worktree", "remove", "--force", dir); err != nil {
					return fmt.Errorf("remove worktree %s: %w", dir, err)
				}
			}
		}
	}
	out, err := r.run(ctx, r.Root, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return err
	}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if issueName(name, iid) {
			// 分支已合入集成分支（本地分支），-d 会因未上游合并失败，用 -D。
			if _, err := r.run(ctx, r.Root, "branch", "-D", name); err != nil {
				return fmt.Errorf("delete branch %s: %w", name, err)
			}
		}
	}
	return nil
}

// HeadCommit 返回目录当前 HEAD 的完整提交 SHA。
func (r *Repo) HeadCommit(ctx context.Context, dir string) (string, error) {
	out, err := r.run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ConflictError 表示 merge 因冲突失败，Files 是冲突文件列表；
// 集成分支已 abort 回到干净状态，由调用方让 agent 解决冲突后重试。
type ConflictError struct {
	Files []string
}

// Error 返回冲突描述。
func (e *ConflictError) Error() string {
	return "merge conflicts in: " + strings.Join(e.Files, " ")
}

// Merge 把子任务分支合入集成分支（在集成 worktree 中执行）。
// 冲突时先取冲突文件列表再 abort，返回 *ConflictError；
// 非冲突失败返回普通错误。
func (r *Repo) Merge(ctx context.Context, integrationDir, branch string) error {
	if _, err := r.run(ctx, integrationDir, "merge", "--no-ff", "--no-edit", branch); err == nil {
		return nil
	} else {
		conflicts, _ := r.run(ctx, integrationDir, "diff", "--name-only", "--diff-filter=U")
		_, _ = r.run(ctx, integrationDir, "merge", "--abort")
		files := strings.Fields(strings.TrimSpace(conflicts))
		if len(files) == 0 {
			return fmt.Errorf("merge %s: %w", branch, err)
		}
		return &ConflictError{Files: files}
	}
}

// SyncBase 把上游 base 分支（origin/<base>）合入集成分支，
// 在集成 worktree 中执行。冲突时 abort 并返回 *ConflictError；
// 调用方决定是否继续（任务分支合并不受影响）。
func (r *Repo) SyncBase(ctx context.Context, integrationDir, base string) error {
	if _, err := r.run(ctx, integrationDir, "fetch", "origin", base); err != nil {
		return fmt.Errorf("fetch origin %s: %w", base, err)
	}
	ref := "origin/" + base
	// 已是祖先则无操作（输出 "Already up to date."）。
	if _, err := r.run(ctx, integrationDir, "merge", "--no-edit", ref); err == nil {
		return nil
	} else {
		conflicts, _ := r.run(ctx, integrationDir, "diff", "--name-only", "--diff-filter=U")
		_, _ = r.run(ctx, integrationDir, "merge", "--abort")
		files := strings.Fields(strings.TrimSpace(conflicts))
		if len(files) == 0 {
			return fmt.Errorf("merge %s: %w", ref, err)
		}
		return &ConflictError{Files: files}
	}
}

// Push 推送集成分支到 origin。
func (r *Repo) Push(ctx context.Context, integrationDir, branch string) error {
	_, err := r.run(ctx, integrationDir, "push", "-u", "origin", branch)
	return err
}

// OriginCoordinate 从 origin remote URL 解析 forge 主机与项目路径，
// 支持 scp（git@host:path.git）、ssh://、https://、http:// 形式；
// 本地路径 remote 没有主机，返回错误。
func (r *Repo) OriginCoordinate(ctx context.Context) (host, path string, err error) {
	raw, err := r.run(ctx, r.Root, "remote", "get-url", "origin")
	if err != nil {
		return "", "", err
	}
	return parseRemoteURL(strings.TrimSpace(raw))
}

// parseRemoteURL 解析 remote URL 为 (host, path)。
func parseRemoteURL(raw string) (string, string, error) {
	if raw == "" {
		return "", "", fmt.Errorf("origin remote URL is empty")
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Scheme != "file" {
		host := u.Hostname()
		path := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
		path = strings.TrimSuffix(path, "/")
		if host == "" || path == "" {
			return "", "", fmt.Errorf("origin remote %q has no host or path", raw)
		}
		return host, path, nil
	}
	// scp 形式：[user@]host:path[.git]
	if i := strings.Index(raw, ":"); i > 0 {
		hostPart := raw[:i]
		path := strings.TrimSuffix(raw[i+1:], ".git")
		path = strings.TrimSuffix(path, "/")
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		if hostPart != "" && path != "" && !strings.Contains(hostPart, "/") {
			return hostPart, path, nil
		}
	}
	return "", "", fmt.Errorf("origin remote %q is not a host URL", raw)
}
