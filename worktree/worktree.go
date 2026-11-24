// Package worktree 由 Go 代码直接管理 git worktree 与分支（不经 agent）：
// spec 的集成分支一个 worktree，每个子任务一个 issue-<iid> 分支 worktree。
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func exitCode(err error, want int) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == want
}

func (r *Repo) fetchOrigin(ctx context.Context) error {
	if _, err := r.run(ctx, r.Root, "fetch", "origin"); err != nil {
		return fmt.Errorf("fetch origin: %w", err)
	}
	return nil
}

// remoteHeads 以 ls-remote 列出 origin 的真实分支：本地 refs/remotes 会残留
// 已删除的分支（fetch 不带 --prune），窄 fetch refspec 下又会缺少真实分支。
func (r *Repo) remoteHeads(ctx context.Context) (map[string]string, error) {
	out, err := r.run(ctx, r.Root, "ls-remote", "--heads", "origin")
	if err != nil {
		return nil, fmt.Errorf("list origin branches: %w", err)
	}
	heads := map[string]string{}
	for line := range strings.Lines(out) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok && name != "" {
			heads[name] = sha
		}
	}
	return heads, nil
}

func matchedHeads(heads map[string]string, iid int) []string {
	var names []string
	for name := range heads {
		if issueName(name, iid) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func (r *Repo) localBranches(ctx context.Context, iid int) ([]string, error) {
	out, err := r.run(ctx, r.Root, "for-each-ref", "--format=%(refname:lstrip=2)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if issueName(name, iid) {
			names = append(names, name)
		}
	}
	return names, nil
}

func (r *Repo) issueDirs(iid int) []string {
	entries, err := os.ReadDir(r.TreeDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		dir := filepath.Join(r.TreeDir, e.Name())
		if e.IsDir() && issueName(e.Name(), iid) {
			if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
				dirs = append(dirs, dir)
			}
		}
	}
	slices.Sort(dirs)
	return dirs
}

// findIssueDir 在 TreeDir 下查找已存在的 issue-<iid>(-*)? worktree 目录。
func (r *Repo) findIssueDir(iid int) string {
	if dirs := r.issueDirs(iid); len(dirs) > 0 {
		return dirs[0]
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

// baseRef 把基点名解析为可建分支的 ref：优先远程跟踪 ref，避免本地
// 同名分支/tag 造成歧义。
func (r *Repo) baseRef(ctx context.Context, base string) string {
	if _, err := r.run(ctx, r.Root, "rev-parse", "--verify", remoteRef(base)); err == nil {
		return remoteRef(base)
	}
	return base
}

// chooseIntegration 决定集成分支名，并返回选中的真实远程分支 SHA（空表示
// 没有远程候选）。want 非空时只认该确切名字；否则按 issue-<spec>(-*)?
// 选远程唯一候选，多个候选或本地匹配名与它不同都报错，不交互。
func chooseIntegration(spec int, want string, heads map[string]string, branches, dirs []string,
	check func(dir, branch string) error) (name, sha string, err error) {
	if want != "" {
		return want, heads[want], nil
	}
	candidates := matchedHeads(heads, spec)
	if len(candidates) > 1 {
		return "", "", fmt.Errorf("multiple origin branches match issue-%d: %s; keep one or set the integration branch in config",
			spec, strings.Join(candidates, ", "))
	}
	if len(candidates) == 0 {
		if len(dirs) > 0 {
			branch := filepath.Base(dirs[0])
			if err := check(dirs[0], branch); err != nil {
				return "", "", err
			}
			return branch, "", nil
		}
		if len(branches) > 0 {
			return branches[0], "", nil
		}
		return IntegrationBranch(spec), "", nil
	}
	remote := candidates[0]
	for _, dir := range dirs {
		if filepath.Base(dir) != remote {
			return "", "", fmt.Errorf("local worktree %s and origin/%s both match issue-%d with different names; "+
				"align them by hand (or set the integration branch in config) and rerun", dir, remote, spec)
		}
		if err := check(dir, remote); err != nil {
			return "", "", err
		}
	}
	for _, branch := range branches {
		if branch != remote {
			return "", "", fmt.Errorf("local branch %s and origin/%s both match issue-%d with different names; "+
				"align them by hand (or set the integration branch in config) and rerun", branch, remote, spec)
		}
	}
	return remote, heads[remote], nil
}

func resolvePath(dir, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

func (r *Repo) commonDir(ctx context.Context, dir string) (string, error) {
	out, err := r.run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return resolvePath(dir, strings.TrimSpace(out)), nil
}

// TopLevel 返回 dir 所属 git 仓库的顶层目录。它用来确认用户给的
// 目录本身是不是主 checkout：git 在子目录里也能跑，remote 会一路
// 查到父仓库，光看 origin 无法区分。
func (r *Repo) TopLevel(ctx context.Context, dir string) (string, error) {
	out, err := r.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	top := strings.TrimSpace(out)
	if top == "" {
		return "", fmt.Errorf("%s is not inside a git repository", dir)
	}
	return resolvePath(dir, top), nil
}

// verifyWorktree 确认 dir 是 r.Root 仓库中停在 refs/heads/<branch> 的
// worktree。普通仓库、别的仓库、detached 或停在其它分支都报错，且不做
// 任何修改。
func (r *Repo) verifyWorktree(ctx context.Context, dir, branch string) error {
	want := "refs/heads/" + branch
	out, err := r.run(ctx, dir, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return fmt.Errorf("directory %s is not a worktree on a branch (detached HEAD); it cannot hold %s", dir, branch)
	}
	if got := strings.TrimSpace(out); got != want {
		return fmt.Errorf("directory %s is on %s, not %s; move it by hand and rerun", dir, got, want)
	}
	rootCommon, err := r.commonDir(ctx, r.Root)
	if err != nil {
		return err
	}
	dirCommon, err := r.commonDir(ctx, dir)
	if err != nil {
		return err
	}
	if dirCommon != rootCommon {
		return fmt.Errorf("directory %s belongs to another repository (%s, want %s)", dir, dirCommon, rootCommon)
	}
	return nil
}

func remoteRef(name string) string {
	return "refs/remotes/origin/" + name
}

func (r *Repo) ensureRemoteRef(ctx context.Context, name, sha string) error {
	out, err := r.run(ctx, r.Root, "rev-parse", "--verify", "--quiet", remoteRef(name))
	if err == nil && strings.TrimSpace(out) == sha {
		return nil
	}
	spec := fmt.Sprintf("+refs/heads/%s:%s", name, remoteRef(name))
	if _, err := r.run(ctx, r.Root, "fetch", "--refmap=", "origin", spec); err != nil {
		return fmt.Errorf("fetch origin %s: %w", name, err)
	}
	return nil
}

func (r *Repo) ensureWorktree(ctx context.Context, branch string) (string, error) {
	dir := filepath.Join(r.TreeDir, branch)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if err := r.verifyWorktree(ctx, dir, branch); err != nil {
			return "", err
		}
		return dir, nil
	}
	if _, err := r.run(ctx, r.Root, "worktree", "add", dir, branch); err != nil {
		return "", fmt.Errorf("add integration worktree %s: %w", dir, err)
	}
	return dir, nil
}

func (r *Repo) newBranch(ctx context.Context, branch, ref string) error {
	if _, err := r.run(ctx, r.Root, "branch", branch, ref); err != nil {
		return fmt.Errorf("create integration branch %s: %w", branch, err)
	}
	return nil
}

func (r *Repo) trackBranch(ctx context.Context, branch string) error {
	spec := fmt.Sprintf("remote.origin.fetch=+refs/heads/%s:%s", branch, remoteRef(branch))
	// 优先 --track；窄 refspec 下它不认可该跟踪 ref，此时仅先建分支。
	if _, err := r.run(ctx, r.Root, "-c", spec, "branch", "--track", branch, remoteRef(branch)); err == nil {
		return nil
	}
	if _, err := r.run(ctx, r.Root, "branch", branch, remoteRef(branch)); err != nil {
		return fmt.Errorf("create integration branch %s: %w", branch, err)
	}
	return nil
}

func (r *Repo) syncBranch(ctx context.Context, dir, branch string) error {
	if _, err := r.run(ctx, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		return fmt.Errorf("worktree %s has an unfinished merge; finish or abort it by hand and rerun", dir)
	} else if !exitCode(err, 1) {
		return fmt.Errorf("check merge state in %s: %w", dir, err)
	}
	out, err := r.run(ctx, dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("check worktree %s: %w", dir, err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("worktree %s has uncommitted changes; commit or stash them before merging %s", dir, remoteRef(branch))
	}
	if _, err := r.run(ctx, dir, "merge-base", "HEAD", remoteRef(branch)); err != nil {
		if exitCode(err, 1) {
			return fmt.Errorf("local %s and %s have no common ancestor; merge them by hand and rerun", branch, remoteRef(branch))
		}
		return fmt.Errorf("compare %s with %s: %w", branch, remoteRef(branch), err)
	}
	_, mergeErr := r.run(ctx, dir, "merge", "--ff", "--no-edit", "--no-squash", remoteRef(branch))
	if mergeErr == nil {
		return nil
	}
	files, _ := r.run(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	var msg string
	if list := strings.Fields(files); len(list) > 0 {
		msg = fmt.Sprintf("merge %s into %s conflicts in: %s", remoteRef(branch), branch, strings.Join(list, " "))
	} else {
		msg = fmt.Sprintf("merge %s into %s: %v", remoteRef(branch), branch, mergeErr)
	}
	if _, err := r.run(ctx, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		return fmt.Errorf("%s; nothing was changed", msg)
	}
	if _, err := r.run(ctx, dir, "merge", "--abort"); err != nil {
		return fmt.Errorf("%s; merge --abort also failed, resolve it by hand in %s: %w", msg, dir, err)
	}
	return fmt.Errorf("%s; merge aborted, resolve by hand and rerun", msg)
}

func (r *Repo) applyIntegration(ctx context.Context, name, sha, baseName string) (branch, dir, base string, err error) {
	branch = name
	dir = filepath.Join(r.TreeDir, name)
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
		if err := r.verifyWorktree(ctx, dir, name); err != nil {
			return "", "", "", err
		}
	}
	if !r.branchExists(ctx, name) {
		if sha != "" {
			err = r.trackBranch(ctx, name)
		} else {
			err = r.newBranch(ctx, name, r.baseRef(ctx, baseName))
		}
		if err != nil {
			return "", "", "", err
		}
	}
	if dir, err = r.ensureWorktree(ctx, name); err != nil {
		return "", "", "", err
	}
	if sha != "" {
		if err := r.syncBranch(ctx, dir, name); err != nil {
			return "", "", "", err
		}
		if err := r.setUpstream(ctx, name); err != nil {
			return "", "", "", err
		}
	}
	return name, dir, baseName, nil
}

func (r *Repo) setUpstream(ctx context.Context, name string) error {
	if out, err := r.run(ctx, r.Root, "rev-parse", "--abbrev-ref", name+"@{upstream}"); err == nil && strings.TrimSpace(out) == "origin/"+name {
		return nil
	}
	spec := fmt.Sprintf("remote.origin.fetch=+refs/heads/%s:%s", name, remoteRef(name))
	if _, err := r.run(ctx, r.Root, "-c", spec, "branch", "--set-upstream-to="+remoteRef(name), name); err != nil {
		return fmt.Errorf("set upstream of %s to %s: %w", name, remoteRef(name), err)
	}
	return nil
}

// EnsureIntegration 确保 spec 的集成分支与它的 worktree 存在，
// 返回分支名、worktree 目录与 MR 目标基点名。
// 候选以 fetch origin 后的 ls-remote 为准（本地 refs/remotes 会残留已删
// 分支、窄 refspec 下又缺真实分支）；显式 want 只认该确切名字，否则选
// issue-<spec>(-*)? 的远程唯一候选并同步远端，多个候选或本地匹配名与
// 远程名不同则报错；远程无候选时复用本地匹配 worktree/分支，都没有才
// 按 main > master > origin/HEAD 选基点建默认名。同步拒绝不干净或有
// 未结束 merge 的 worktree，只在有共同祖先时合并。
func (r *Repo) EnsureIntegration(ctx context.Context, spec int, base, want string) (branch, dir, baseName string, err error) {
	if err = os.MkdirAll(r.TreeDir, 0o755); err != nil {
		return "", "", "", fmt.Errorf("create tree dir %s: %w", r.TreeDir, err)
	}
	// 先 fetch 再定 baseName，避免旧 remote-tracking ref 导致误选 base。
	if err = r.fetchOrigin(ctx); err != nil {
		return "", "", "", err
	}
	baseName = r.baseName(ctx, base)
	heads, err := r.remoteHeads(ctx)
	if err != nil {
		return "", "", "", err
	}
	branches, err := r.localBranches(ctx, spec)
	if err != nil {
		return "", "", "", err
	}
	check := func(dir, branch string) error { return r.verifyWorktree(ctx, dir, branch) }
	name, sha, err := chooseIntegration(spec, want, heads, branches, r.issueDirs(spec), check)
	if err != nil {
		return "", "", "", err
	}
	if sha != "" {
		if err := r.ensureRemoteRef(ctx, name, sha); err != nil {
			return "", "", "", err
		}
	}
	return r.applyIntegration(ctx, name, sha, baseName)
}

// Create 为子任务从 intBranch（实际集成分支名，含描述后缀/覆盖名）
// 建 issue-<iid>(-*)? 分支与 worktree。已有匹配的 worktree 目录直接
// 复用；有匹配分支复用该分支；都没有则从 intBranch 新建 issue-<iid>。
// 返回目录与实际分支名。
func (r *Repo) Create(ctx context.Context, intBranch string, iid int) (dir, branch string, err error) {
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
	if _, err := r.run(ctx, r.Root, "worktree", "add", dir, "-b", branch, intBranch); err != nil {
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
