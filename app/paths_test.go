package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit 在 dir 里跑一条 git 命令，失败即结束测试。
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// 全部路径都能自动解析：工作目录、仓库顶层、默认 worktree 目录。
func TestPreviewPathsDiscoversAll(t *testing.T) {
	repo := gitRepo(t)
	p, err := PreviewPaths(context.Background(), Options{WorkDir: repo})
	if err != nil {
		t.Fatalf("PreviewPaths = %v", err)
	}
	if !p.Ready() {
		t.Fatalf("preview = %+v, want all three resolved", p)
	}
	if p.Paths.WorkDir != repo {
		t.Errorf("WorkDir = %q, want %q", p.Paths.WorkDir, repo)
	}
	if p.Paths.RepoDir != repo {
		t.Errorf("RepoDir = %q, want the repository's top level %q", p.Paths.RepoDir, repo)
	}
	if want := filepath.Join(repo, ".worktrees"); p.Paths.TreeDir != want {
		t.Errorf("TreeDir = %q, want %q", p.Paths.TreeDir, want)
	}
}

// 工作目录不是仓库：仓库目录报错，但工作目录仍保留，界面只补填仓库那
// 一项。
func TestPreviewPathsKeepsWorkDirWhenRepoFails(t *testing.T) {
	work := t.TempDir()
	p, err := PreviewPaths(context.Background(), Options{WorkDir: work})
	if err != nil {
		t.Fatalf("PreviewPaths = %v, want the work dir resolved with a repo error in the preview", err)
	}
	if p.Paths.WorkDir != work {
		t.Fatalf("WorkDir = %q, want %q kept", p.Paths.WorkDir, work)
	}
	if p.RepoErr == nil {
		t.Fatal("RepoErr = nil, want the repository failure reported separately")
	}
	if p.Paths.RepoDir != "" || p.Paths.TreeDir != "" {
		t.Errorf("Pp.. = %+v, want repo and tree left empty so the form can fill them", p.Paths)
	}
	if p.TreeErr != nil {
		t.Error("TreeErr set while the repository is still unknown")
	}
	if p.Ready() {
		t.Error("Ready() = true with a missing repository")
	}
	if !IsRepositoryError(p.RepoErr) {
		t.Fatalf("RepoErr = %v (%T), want a *RepositoryError so the UI can offer the repo field", p.RepoErr, p.RepoErr)
	}
}

// 显式仓库目录指向仓库里的子目录时必须在预览阶段就被拒绝：UI 的严格
// 顶层校验不允许悄悄把父仓库认下来。
func TestPreviewPathsRejectsSubdirectoryRepo(t *testing.T) {
	repo := gitRepo(t)
	sub := filepath.Join(repo, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := PreviewPaths(context.Background(), Options{WorkDir: repo, RepoDir: sub})
	if err != nil {
		t.Fatalf("PreviewPaths = %v", err)
	}
	if p.Paths.RepoDir != "" {
		t.Fatalf("RepoDir = %q, want the subdirectory rejected", p.Paths.RepoDir)
	}
	if p.RepoErr == nil || !strings.Contains(p.RepoErr.Error(), "top level") {
		t.Fatalf("RepoErr = %v, want it to name the top-level requirement", p.RepoErr)
	}
	if p.Paths.WorkDir != repo {
		t.Errorf("WorkDir = %q, want the work dir kept", p.Paths.WorkDir)
	}
}

// 显式仓库目录是顶层且存在既有 worktree 时，TreeDir 复用同一个父目录。
func TestPreviewPathsUsesExistingWorktreeParent(t *testing.T) {
	repo := gitRepo(t)
	tree := filepath.Join(t.TempDir(), "trees")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "worktree", "add", "-b", "issue-1", filepath.Join(tree, "issue-1"))
	p, err := PreviewPaths(context.Background(), Options{WorkDir: repo, RepoDir: repo})
	if err != nil {
		t.Fatalf("PreviewPaths = %v", err)
	}
	if p.Paths.TreeDir != tree {
		t.Errorf("TreeDir = %q, want the inferred %q", p.Paths.TreeDir, tree)
	}
	if !p.Ready() {
		t.Errorf("preview = %+v, want ready", p)
	}
}

// 配置本身不可用是普通失败：PreviewPaths 返回错误而不是仓库错误，界面
// 因此不会误弹仓库提示。
func TestPreviewPathsReportsConfigError(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("parallelism = \"many\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewPaths(context.Background(), Options{WorkDir: dir, ConfigPath: bad}); err == nil {
		t.Fatal("PreviewPaths accepted a broken config")
	} else if IsRepositoryError(err) {
		t.Fatalf("err = %v, want a plain config failure, not a repository error", err)
	}
}
