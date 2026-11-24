package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitDo 在 dir 中执行 git，失败即终止测试，返回去空白的输出。
func gitDo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitFails 报告 dir 中的 git 命令是否失败。
func gitFails(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() != nil
}

// fixture 是离线测试环境：裸仓库作 origin，work 是主 checkout，
// collab 是独立的协作者 checkout（新分支只从那边推送，work 不会因为
// push 自动得到 remote-tracking ref）。
type fixture struct {
	*Repo
	bare   string
	collab string
}

// newFixture 建 bare origin + seed 推 main + work 克隆 + collab 克隆。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "origin.git")
	gitDo(t, dir, "init", "--bare", bare)
	seed := filepath.Join(dir, "seed")
	gitDo(t, dir, "init", "-b", "main", seed)
	gitDo(t, seed, "config", "user.email", "test@test")
	gitDo(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, seed, "add", "base.txt")
	gitDo(t, seed, "commit", "-m", "init")
	gitDo(t, seed, "push", bare, "main")
	gitDo(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return &fixture{Repo: newClone(t, bare, dir, "work"), bare: bare, collab: newClone(t, bare, dir, "collab").Root}
}

// newClone 克隆 bare 到 dir/name 并返回其 Repo，worktree 目录为 .wt。
func newClone(t *testing.T, bare, dir, name string) *Repo {
	t.Helper()
	root := filepath.Join(dir, name)
	gitDo(t, dir, "clone", bare, root)
	gitDo(t, root, "config", "user.email", "test@test")
	gitDo(t, root, "config", "user.name", "test")
	return NewRepo(root, filepath.Join(root, ".wt"))
}

// pushFile 由协作者 checkout 在 branch 上（不存在则从 origin/main 起）
// 造一个提交并推到 origin，返回该提交 SHA。
func (f *fixture) pushFile(t *testing.T, branch, file, content string) string {
	t.Helper()
	gitDo(t, f.collab, "fetch", "origin")
	if gitFails(t, f.collab, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch) {
		gitDo(t, f.collab, "checkout", "-q", "-B", branch, "origin/main")
	} else {
		gitDo(t, f.collab, "checkout", "-q", "-B", branch, "origin/"+branch)
	}
	if err := os.WriteFile(filepath.Join(f.collab, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, f.collab, "add", file)
	gitDo(t, f.collab, "commit", "-m", "commit for "+branch)
	sha := gitDo(t, f.collab, "rev-parse", "HEAD")
	gitDo(t, f.collab, "push", "origin", branch)
	gitDo(t, f.collab, "checkout", "-q", "main")
	gitDo(t, f.collab, "reset", "-q", "--hard", "origin/main")
	return sha
}

// pushBranch 同 pushFile，文件内容取文件名。
func (f *fixture) pushBranch(t *testing.T, branch, file string) string {
	t.Helper()
	return f.pushFile(t, branch, file, file+"\n")
}

// commitOn 为 work 中已存在的分支 branch 提交一个文件，返回提交 SHA；
// 临时 worktree 随即删除，主 checkout 不受影响。
func commitOn(t *testing.T, r *Repo, branch, file, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wt")
	gitDo(t, r.Root, "worktree", "add", dir, branch)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitDo(t, dir, "add", file)
	gitDo(t, dir, "commit", "-m", "commit on "+branch)
	sha := gitDo(t, dir, "rev-parse", "HEAD")
	gitDo(t, r.Root, "worktree", "remove", "--force", dir)
	return sha
}

// rootCommit 造一个无父提交（与 main 无共同祖先），返回其 SHA。
func rootCommit(t *testing.T, dir, msg string) string {
	t.Helper()
	return gitDo(t, dir, "commit-tree", gitDo(t, dir, "mktree"), "-m", msg)
}

// TestEnsureIntegrationRemoteSuffix 远程唯一 issue-<spec>-<描述> 分支：
// 本地没有任何 remote-tracking 缓存时也能发现并跟踪它。
func TestEnsureIntegrationRemoteSuffix(t *testing.T) {
	f := newFixture(t)
	sha := f.pushBranch(t, "issue-5-fix-thing", "remote.txt")
	ctx := context.Background()
	if !gitFails(t, f.Root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/issue-5-fix-thing") {
		t.Fatal("fixture must not have a cached remote-tracking ref")
	}
	branch, dir, _, err := f.EnsureIntegration(ctx, 5, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-5-fix-thing" {
		t.Fatalf("branch = %q, want issue-5-fix-thing", branch)
	}
	head, err := f.HeadCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want remote %s", head, sha)
	}
	if up := gitDo(t, f.Root, "rev-parse", "--abbrev-ref", branch+"@{upstream}"); up != "origin/"+branch {
		t.Fatalf("upstream = %q, want origin/%s", up, branch)
	}
	if _, err := os.Stat(filepath.Join(dir, "remote.txt")); err != nil {
		t.Fatalf("worktree lacks remote content: %v", err)
	}
}

// TestEnsureIntegrationStaleRemoteRef 远端已删除但本地仍有 stale
// remote-tracking ref 时，不能把它当成真实远程分支。
func TestEnsureIntegrationStaleRemoteRef(t *testing.T) {
	f := newFixture(t)
	f.pushBranch(t, "issue-33-x", "gone.txt")
	gitDo(t, f.Root, "fetch", "origin")
	if gitFails(t, f.Root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/issue-33-x") {
		t.Fatal("expected a cached remote-tracking ref before deletion")
	}
	gitDo(t, f.collab, "push", "origin", "--delete", "issue-33-x")
	gitDo(t, f.Root, "branch", "issue-33-local", "origin/main")
	branch, _, _, err := f.EnsureIntegration(context.Background(), 33, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-33-local" {
		t.Fatalf("branch = %q, want issue-33-local (stale ref must not be a candidate)", branch)
	}
	if gitFails(t, f.Root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/issue-33-x") {
		t.Fatal("stale remote-tracking ref should be left untouched")
	}
}

// TestEnsureIntegrationNarrowRefspec 窄 fetch refspec 下真实远端分支没有
// 本地缓存：仍要发现它、记录 upstream，且不改长期 fetch 配置。
func TestEnsureIntegrationNarrowRefspec(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(filepath.Dir(f.bare), "narrow")
	gitDo(t, filepath.Dir(f.bare), "clone", f.bare, root)
	narrow := NewRepo(root, filepath.Join(root, ".wt"))
	gitDo(t, root, "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	gitDo(t, root, "fetch", "origin")
	sha := f.pushBranch(t, "issue-34-x", "narrow.txt")
	ctx := context.Background()
	if !gitFails(t, narrow.Root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/issue-34-x") {
		t.Fatal("narrow refspec must not have cached the branch")
	}
	branch, dir, _, err := narrow.EnsureIntegration(ctx, 34, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-34-x" {
		t.Fatalf("branch = %q, want issue-34-x", branch)
	}
	head, err := narrow.HeadCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want remote %s", head, sha)
	}
	if remote := gitDo(t, narrow.Root, "config", "branch."+branch+".remote"); remote != "origin" {
		t.Fatalf("branch.%s.remote = %q, want origin", branch, remote)
	}
	if merge := gitDo(t, narrow.Root, "config", "branch."+branch+".merge"); merge != "refs/heads/"+branch {
		t.Fatalf("branch.%s.merge = %q, want refs/heads/%s", branch, merge, branch)
	}
	if spec := gitDo(t, narrow.Root, "config", "remote.origin.fetch"); spec != "+refs/heads/main:refs/remotes/origin/main" {
		t.Fatalf("long-term fetch refspec changed: %q", spec)
	}
}

// TestIssueNameExact 边界匹配：issue-<spec>0 不算 issue <spec> 的分支。
func TestIssueNameExact(t *testing.T) {
	cases := map[string]bool{
		"issue-35":       true,
		"issue-35-desc":  true,
		"issue-350":      false,
		"issue-350-desc": false,
		"issue-3":        false,
	}
	for name, want := range cases {
		if got := issueName(name, 35); got != want {
			t.Errorf("issueName(%q, 35) = %v, want %v", name, got, want)
		}
	}
}

// TestEnsureIntegrationExactNameOnly 远程同时有 issue-35 与 issue-350 时
// 只按 exact issue-35 选候候选。
func TestEnsureIntegrationExactNameOnly(t *testing.T) {
	f := newFixture(t)
	sha := f.pushBranch(t, "issue-35", "exact.txt")
	f.pushBranch(t, "issue-350", "other.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 35, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-35" {
		t.Fatalf("branch = %q, want issue-35", branch)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want %s", head, sha)
	}
}

// TestEnsureIntegrationRemoteNone 远程没有候选：按原规则建默认名分支。
func TestEnsureIntegrationRemoteNone(t *testing.T) {
	f := newFixture(t)
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 6, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := IntegrationBranch(6); branch != want {
		t.Fatalf("branch = %q, want %q", branch, want)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != gitDo(t, f.Root, "rev-parse", "origin/main") {
		t.Fatalf("HEAD = %s, want origin/main", head)
	}
}

// TestEnsureIntegrationRemoteMultiple 远程多个候选：报错并列出候选名，
// 不建任何分支。
func TestEnsureIntegrationRemoteMultiple(t *testing.T) {
	f := newFixture(t)
	f.pushBranch(t, "issue-7-b", "b.txt")
	f.pushBranch(t, "issue-7-a", "a.txt")
	_, _, _, err := f.EnsureIntegration(context.Background(), 7, "", "")
	if err == nil {
		t.Fatal("want error for multiple remote candidates")
	}
	for _, name := range []string{"issue-7-a", "issue-7-b"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not list %q", err, name)
		}
	}
	if !gitFails(t, f.Root, "rev-parse", "--verify", "--quiet", "refs/heads/"+IntegrationBranch(7)) {
		t.Fatal("no branch should be created on candidate ambiguity")
	}
}

// TestEnsureIntegrationWantIgnoresOtherRemote 显式配置名：远程其它
// issue-<spec> 前缀分支不算候选，按配置名从 base 新建。
func TestEnsureIntegrationWantIgnoresOtherRemote(t *testing.T) {
	f := newFixture(t)
	f.pushBranch(t, "issue-8-other", "other.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 8, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "integration" {
		t.Fatalf("branch = %q, want integration", branch)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != gitDo(t, f.Root, "rev-parse", "origin/main") {
		t.Fatalf("HEAD = %s, want origin/main", head)
	}
}

// TestEnsureIntegrationWantReusesExactRemote 显式配置名在远程存在：
// 复用该远程分支，不选其它模糊候选。
func TestEnsureIntegrationWantReusesExactRemote(t *testing.T) {
	f := newFixture(t)
	sha := f.pushBranch(t, "integration", "integration.txt")
	f.pushBranch(t, "issue-9-other", "other.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 9, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "integration" {
		t.Fatalf("branch = %q, want integration", branch)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want remote %s", head, sha)
	}
}

// TestEnsureIntegrationWantIgnoresLocalBranch 显式配置名时本地
// issue-<spec>-<描述> 分支不算候选。
func TestEnsureIntegrationWantIgnoresLocalBranch(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-12-something", "origin/main")
	f.pushBranch(t, "issue-12-remote", "remote.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 12, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "integration" || dir != filepath.Join(f.TreeDir, "integration") {
		t.Fatalf("got (%q, %q), want integration under tree dir", branch, dir)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != gitDo(t, f.Root, "rev-parse", "origin/main") {
		t.Fatalf("HEAD = %s, want origin/main", head)
	}
}

// TestEnsureIntegrationWantIgnoresLocalWorktree 显式配置名时本地
// issue-<spec>-<描述> worktree 不算候选，也不被改动。
func TestEnsureIntegrationWantIgnoresLocalWorktree(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(f.TreeDir, "issue-13-something")
	gitDo(t, f.Root, "worktree", "add", other, "-b", "issue-13-something", "origin/main")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 13, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "integration" || dir != filepath.Join(f.TreeDir, "integration") {
		t.Fatalf("got (%q, %q), want integration under tree dir", branch, dir)
	}
	if _, err := os.Stat(filepath.Join(other, ".git")); err != nil {
		t.Fatalf("existing spec worktree disturbed: %v", err)
	}
}

// TestEnsureIntegrationWantReusesLocalBranch 显式配置名的本地分支已
// 存在：直接复用该分支。
func TestEnsureIntegrationWantReusesLocalBranch(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "integration", "origin/main")
	sha := commitOn(t, f.Repo, "integration", "local.txt", "local\n")
	f.pushBranch(t, "issue-14-remote", "remote.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 14, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "integration" {
		t.Fatalf("branch = %q, want integration", branch)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want local %s", head, sha)
	}
}

// TestEnsureIntegrationWantReusesNamedWorktree 显式配置名的 worktree
// 已存在：直接复用该目录。
func TestEnsureIntegrationWantReusesNamedWorktree(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.TreeDir, "integration")
	gitDo(t, f.Root, "worktree", "add", dir, "-b", "integration", "origin/main")
	branch, got, _, err := f.EnsureIntegration(context.Background(), 15, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "integration" || got != dir {
		t.Fatalf("got (%q, %q), want (integration, %q)", branch, got, dir)
	}
}

// TestEnsureIntegrationNameConflict 本地匹配分支名与唯一远端候选名不同：
// 报错列出两者，本地分支不动。
func TestEnsureIntegrationNameConflict(t *testing.T) {
	f := newFixture(t)
	local := gitDo(t, f.Root, "rev-parse", "origin/main")
	gitDo(t, f.Root, "branch", "issue-20-local", local)
	f.pushBranch(t, "issue-20-remote", "remote.txt")
	_, _, _, err := f.EnsureIntegration(context.Background(), 20, "", "")
	if err == nil {
		t.Fatal("want error for local/remote name conflict")
	}
	for _, want := range []string{"issue-20-local", "origin/issue-20-remote"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if got := gitDo(t, f.Root, "rev-parse", "issue-20-local"); got != local {
		t.Fatalf("local branch moved to %s, want %s", got, local)
	}
}

// TestEnsureIntegrationNameConflictDir 遍历所有匹配目录：第二个不同名
// 目录也要报出来，不能只查第一个。
func TestEnsureIntegrationNameConflictDir(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(f.TreeDir, "issue-21-zother")
	gitDo(t, f.Root, "worktree", "add", other, "-b", "issue-21-zother", "origin/main")
	same := filepath.Join(f.TreeDir, "issue-21-remote")
	f.pushBranch(t, "issue-21-remote", "remote.txt")
	gitDo(t, f.Root, "worktree", "add", same, "-b", "issue-21-remote", "origin/main")
	_, _, _, err := f.EnsureIntegration(context.Background(), 21, "", "")
	if err == nil {
		t.Fatal("want error for the second differing worktree")
	}
	if !strings.Contains(err.Error(), other) {
		t.Fatalf("error %q does not mention %q", err, other)
	}
	if _, statErr := os.Stat(filepath.Join(other, ".git")); statErr != nil {
		t.Fatalf("existing worktree disturbed: %v", statErr)
	}
}

// TestEnsureIntegrationWrongBranchCheckout 目录停在别的分支：报错，HEAD
// 与分支都不被改动。
func TestEnsureIntegrationWrongBranchCheckout(t *testing.T) {
	f := newFixture(t)
	f.pushBranch(t, "issue-30-x", "remote.txt")
	dir := filepath.Join(f.TreeDir, "issue-30-x")
	gitDo(t, f.Root, "worktree", "add", dir, "-b", "issue-30-other", "origin/main")
	before := gitDo(t, dir, "rev-parse", "HEAD")
	_, _, _, err := f.EnsureIntegration(context.Background(), 30, "", "")
	if err == nil {
		t.Fatal("want error for a worktree on another branch")
	}
	if !strings.Contains(err.Error(), "refs/heads/issue-30-other") {
		t.Fatalf("error %q does not name the checked out branch", err)
	}
	if got := gitDo(t, dir, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD = %s, want unchanged %s", got, before)
	}
	if got := gitDo(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/issue-30-other" {
		t.Fatalf("checked out branch = %q, want unchanged", got)
	}
	if got := gitDo(t, f.Root, "rev-parse", "issue-30-other"); got != before {
		t.Fatalf("other branch moved to %s, want %s", got, before)
	}
}

// TestEnsureIntegrationDetachedWorktree 目录处于 detached HEAD：报错且
// HEAD 不变。
func TestEnsureIntegrationDetachedWorktree(t *testing.T) {
	f := newFixture(t)
	f.pushBranch(t, "issue-31-x", "remote.txt")
	dir := filepath.Join(f.TreeDir, "issue-31-x")
	gitDo(t, f.Root, "worktree", "add", "--detach", dir, "origin/main")
	before := gitDo(t, dir, "rev-parse", "HEAD")
	_, _, _, err := f.EnsureIntegration(context.Background(), 31, "", "")
	if err == nil {
		t.Fatal("want error for a detached worktree")
	}
	if !strings.Contains(err.Error(), "detached") {
		t.Fatalf("error %q does not explain the detached HEAD", err)
	}
	if got := gitDo(t, dir, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD = %s, want unchanged %s", got, before)
	}
	if !gitFails(t, f.Root, "rev-parse", "--verify", "--quiet", "refs/heads/issue-31-x") {
		t.Fatal("branch must not be created for a detached directory")
	}
}

// TestEnsureIntegrationSyncFastForward 同名本地分支落后于远端：同步为
// 远端提交。
func TestEnsureIntegrationSyncFastForward(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-16-x", "origin/main")
	sha := f.pushBranch(t, "issue-16-x", "remote.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 16, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-16-x" {
		t.Fatalf("branch = %q, want issue-16-x", branch)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want %s", head, sha)
	}
}

// TestEnsureIntegrationSyncMergeKeepsBoth 本地与远端各有独有提交且有共同
// 祖先：合并后两方文件都在。
func TestEnsureIntegrationSyncMergeKeepsBoth(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-17-x", "origin/main")
	localSha := commitOn(t, f.Repo, "issue-17-x", "local.txt", "local\n")
	remoteSha := f.pushBranch(t, "issue-17-x", "remote.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 17, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-17-x" {
		t.Fatalf("branch = %q, want issue-17-x", branch)
	}
	for _, name := range []string{"local.txt", "remote.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("merged worktree lacks %s: %v", name, err)
		}
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head == localSha || head == remoteSha {
		t.Fatalf("HEAD = %s, want a merge commit", head)
	}
	if parents := gitDo(t, f.Root, "rev-list", "--parents", "-n", "1", head); len(strings.Fields(parents)) != 3 {
		t.Fatalf("merge commit parents = %q, want 2", parents)
	}
}

// TestEnsureIntegrationSyncMergeFFOnly 仓库配了 merge.ff=only 时双方分叉
// 仍要能正常产生 merge commit。
func TestEnsureIntegrationSyncMergeFFOnly(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "config", "merge.ff", "only")
	gitDo(t, f.Root, "branch", "issue-27-x", "origin/main")
	commitOn(t, f.Repo, "issue-27-x", "local.txt", "local\n")
	f.pushBranch(t, "issue-27-x", "remote.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 27, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-27-x" {
		t.Fatalf("branch = %q, want issue-27-x", branch)
	}
	for _, name := range []string{"local.txt", "remote.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("merged worktree lacks %s: %v", name, err)
		}
	}
	if parents := gitDo(t, f.Root, "rev-list", "--parents", "-n", "1", "refs/heads/issue-27-x"); len(strings.Fields(parents)) != 3 {
		t.Fatalf("HEAD parents = %q, want a merge commit", parents)
	}
}

// TestEnsureIntegrationSyncNoCommonAncestor 无共同祖先：报错说明，两边
// 提交都保持。
func TestEnsureIntegrationSyncNoCommonAncestor(t *testing.T) {
	f := newFixture(t)
	localSha := rootCommit(t, f.Root, "unrelated local")
	gitDo(t, f.Root, "branch", "issue-18-x", localSha)
	remoteSha := rootCommit(t, f.collab, "unrelated remote")
	gitDo(t, f.collab, "push", "origin", remoteSha+":refs/heads/issue-18-x")
	_, _, _, err := f.EnsureIntegration(context.Background(), 18, "", "")
	if err == nil {
		t.Fatal("want error for no common ancestor")
	}
	if !strings.Contains(err.Error(), "no common ancestor") {
		t.Fatalf("error %q does not explain the missing common ancestor", err)
	}
	if got := gitDo(t, f.Root, "rev-parse", "issue-18-x"); got != localSha {
		t.Fatalf("local branch = %s, want %s", got, localSha)
	}
	if got := gitDo(t, f.Root, "rev-parse", "refs/remotes/origin/issue-18-x"); got != remoteSha {
		t.Fatalf("remote ref = %s, want %s", got, remoteSha)
	}
}

// TestEnsureIntegrationSyncConflictAborts merge 冲突：abort 后 worktree
// 干净且本地 HEAD 保持原值。
func TestEnsureIntegrationSyncConflictAborts(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-19-x", "origin/main")
	localSha := commitOn(t, f.Repo, "issue-19-x", "base.txt", "local\n")
	dir := filepath.Join(f.TreeDir, "issue-19-x")
	gitDo(t, f.Root, "worktree", "add", dir, "issue-19-x")
	f.pushFile(t, "issue-19-x", "base.txt", "remote\n")
	_, _, _, err := f.EnsureIntegration(context.Background(), 19, "", "")
	if err == nil {
		t.Fatal("want conflict error")
	}
	if !strings.Contains(err.Error(), "base.txt") {
		t.Fatalf("error %q does not list the conflicting file", err)
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("error %q does not report the abort", err)
	}
	if out := gitDo(t, dir, "status", "--porcelain"); out != "" {
		t.Fatalf("worktree not clean after abort: %q", out)
	}
	if head := gitDo(t, dir, "rev-parse", "HEAD"); head != localSha {
		t.Fatalf("HEAD = %s, want original %s", head, localSha)
	}
	if content, readErr := os.ReadFile(filepath.Join(dir, "base.txt")); readErr != nil || string(content) != "local\n" {
		t.Fatalf("local content = %q, %v; want local", content, readErr)
	}
}

// TestEnsureIntegrationSyncPendingMerge 已有未结束的 merge：拒绝且绝不
// abort 用户的操作。
func TestEnsureIntegrationSyncPendingMerge(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-32-x", "origin/main")
	commitOn(t, f.Repo, "issue-32-x", "base.txt", "local\n")
	dir := filepath.Join(f.TreeDir, "issue-32-x")
	gitDo(t, f.Root, "worktree", "add", dir, "issue-32-x")
	remoteSha := f.pushFile(t, "issue-32-x", "base.txt", "remote\n")
	gitDo(t, f.Root, "fetch", "origin")
	if !gitFails(t, dir, "merge", "refs/remotes/origin/issue-32-x") {
		t.Fatal("fixture expects a conflicting merge")
	}
	mergeHead := gitDo(t, dir, "rev-parse", "MERGE_HEAD")
	_, _, _, err := f.EnsureIntegration(context.Background(), 32, "", "")
	if err == nil {
		t.Fatal("want error for an unfinished merge")
	}
	if !strings.Contains(err.Error(), "unfinished merge") {
		t.Fatalf("error %q does not explain the pending merge", err)
	}
	if got := gitDo(t, dir, "rev-parse", "MERGE_HEAD"); got != mergeHead {
		t.Fatalf("MERGE_HEAD = %s, want untouched %s", got, mergeHead)
	}
	if got := gitDo(t, f.Root, "rev-parse", "refs/remotes/origin/issue-32-x"); got != remoteSha {
		t.Fatalf("remote ref = %s, want %s", got, remoteSha)
	}
}

// TestEnsureIntegrationSyncDirtyWorktree 工作区有未提交改动：报错且不动
// 用户内容。
func TestEnsureIntegrationSyncDirtyWorktree(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-26-x", "origin/main")
	localSha := gitDo(t, f.Root, "rev-parse", "issue-26-x")
	dir := filepath.Join(f.TreeDir, "issue-26-x")
	gitDo(t, f.Root, "worktree", "add", dir, "issue-26-x")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.pushBranch(t, "issue-26-x", "remote.txt")
	_, _, _, err := f.EnsureIntegration(context.Background(), 26, "", "")
	if err == nil {
		t.Fatal("want error for dirty worktree")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("error %q does not explain the dirty worktree", err)
	}
	if content, readErr := os.ReadFile(filepath.Join(dir, "base.txt")); readErr != nil || string(content) != "dirty\n" {
		t.Fatalf("user changes lost: %q, %v", content, readErr)
	}
	if head := gitDo(t, dir, "rev-parse", "HEAD"); head != localSha {
		t.Fatalf("HEAD = %s, want %s", head, localSha)
	}
}

// TestEnsureIntegrationSyncExistingWorktree 已有同名 worktree：在该目录内
// 同步远端更新并复用。
func TestEnsureIntegrationSyncExistingWorktree(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-22-x", "origin/main")
	dir := filepath.Join(f.TreeDir, "issue-22-x")
	gitDo(t, f.Root, "worktree", "add", dir, "issue-22-x")
	sha := f.pushBranch(t, "issue-22-x", "remote.txt")
	branch, got, _, err := f.EnsureIntegration(context.Background(), 22, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-22-x" || got != dir {
		t.Fatalf("got (%q, %q), want (issue-22-x, %q)", branch, got, dir)
	}
	head, err := f.HeadCommit(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want %s", head, sha)
	}
	if _, err := os.Stat(filepath.Join(got, "remote.txt")); err != nil {
		t.Fatalf("worktree lacks remote content: %v", err)
	}
}

// TestEnsureIntegrationRemoteGoneReusesLocal 远端没有候选时保留原本地
// 匹配分支/目录的复用行为。
func TestEnsureIntegrationRemoteGoneReusesLocal(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-23-x", "origin/main")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 23, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-23-x" {
		t.Fatalf("branch = %q, want issue-23-x", branch)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("worktree not created for reused branch: %v", err)
	}

	other := filepath.Join(f.TreeDir, "issue-24-y")
	gitDo(t, f.Root, "worktree", "add", other, "-b", "issue-24-y", "origin/main")
	branch, dir, _, err = f.EnsureIntegration(context.Background(), 24, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "issue-24-y" || dir != other {
		t.Fatalf("got (%q, %q), want (issue-24-y, %q)", branch, dir, other)
	}
}

// TestEnsureIntegrationWantSyncsRemote 配置名忽略其它前缀本地对象，并把
// 同名远端更新同步进本地分支。
func TestEnsureIntegrationWantSyncsRemote(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "branch", "issue-25-other", "origin/main")
	otherSha := gitDo(t, f.Root, "rev-parse", "issue-25-other")
	gitDo(t, f.Root, "branch", "integration", "origin/main")
	sha := f.pushBranch(t, "integration", "integration.txt")
	branch, dir, _, err := f.EnsureIntegration(context.Background(), 25, "", "integration")
	if err != nil {
		t.Fatal(err)
	}
	if branch != "integration" {
		t.Fatalf("branch = %q, want integration", branch)
	}
	head, err := f.HeadCommit(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if head != sha {
		t.Fatalf("HEAD = %s, want synced %s", head, sha)
	}
	if got := gitDo(t, f.Root, "rev-parse", "issue-25-other"); got != otherSha {
		t.Fatalf("other branch moved to %s, want %s", got, otherSha)
	}
}

// TestEnsureIntegrationFetchError origin 不可达：返回错误，不当作无远程
// 候选去建新分支。
func TestEnsureIntegrationFetchError(t *testing.T) {
	f := newFixture(t)
	gitDo(t, f.Root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	_, _, _, err := f.EnsureIntegration(context.Background(), 11, "", "")
	if err == nil {
		t.Fatal("want fetch error")
	}
	if !strings.Contains(err.Error(), "fetch origin") {
		t.Fatalf("error %q does not mention fetch origin", err)
	}
	if !gitFails(t, f.Root, "rev-parse", "--verify", "--quiet", "refs/heads/"+IntegrationBranch(11)) {
		t.Fatal("no branch should be created when fetch fails")
	}
}
