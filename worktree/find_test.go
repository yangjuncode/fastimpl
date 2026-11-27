package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestForgeForHost 验证按主机名推断 forge。
func TestForgeForHost(t *testing.T) {
	cases := map[string]string{
		"github.com":         "github",
		"api.github.com":     "github",
		"gitlab.com":         "gitlab",
		"gitlab.corp.com":    "gitlab",
		"git.bfdx.com":       "", // 自建实例推断不出，走默认 gitlab
		"bitbucket.org":      "",
		"GITLAB.COM":         "gitlab",
		"Gitee.COM":          "",
		"mygithub.mirror":    "", // 非 .github.com 后缀不算
		"github.com.evil.io": "",
	}
	for host, want := range cases {
		if got := ForgeForHost(host); got != want {
			t.Errorf("ForgeForHost(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestFindRootMultipleReposError(t *testing.T) {
	base := t.TempDir()
	repoA := filepath.Join(base, "alpha")
	repoB := filepath.Join(base, "sub", "beta")
	for _, r := range []string{repoA, repoB} {
		if err := os.MkdirAll(filepath.Join(r, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, err := FindRoot(base)
	if err == nil {
		t.Fatal("FindRoot succeeded despite multiple repos")
	}
	var multi *MultipleReposError
	if !errors.As(err, &multi) {
		t.Fatalf("FindRoot error = %T (%v), want *MultipleReposError", err, err)
	}
	want := []string{repoA, repoB}
	if !slices.Equal(multi.Repos, want) {
		t.Errorf("Repos = %v, want %v", multi.Repos, want)
	}
	if !strings.Contains(err.Error(), "multiple git repositories found near") {
		t.Errorf("Error() = %q, want backward-compatible message", err.Error())
	}
}

func TestIsRepoRoot(t *testing.T) {
	base := t.TempDir()
	if IsRepoRoot("") || IsRepoRoot(base) {
		t.Fatal("IsRepoRoot reported non-repo dir as repo root")
	}
	// .git 是文件（linked worktree）不算主仓库根。
	linked := filepath.Join(base, "linked")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: /tmp/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsRepoRoot(linked) {
		t.Fatal("IsRepoRoot accepted .git file")
	}
	repo := filepath.Join(base, "main")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsRepoRoot(repo) {
		t.Fatal("IsRepoRoot rejected .git directory")
	}
}

func TestResolveTreeDirMultipleTreeDirsError(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("commit", "--allow-empty", "-m", "init")
	treeA := filepath.Join(base, "trees-a")
	treeB := filepath.Join(base, "trees-b")
	git("worktree", "add", "-b", "wt-a", filepath.Join(treeA, "wt-a"))
	git("worktree", "add", "-b", "wt-b", filepath.Join(treeB, "wt-b"))

	_, err := ResolveTreeDir(context.Background(), repo, "")
	if err == nil {
		t.Fatal("ResolveTreeDir succeeded despite non-uniform worktree parents")
	}
	var multi *MultipleTreeDirsError
	if !errors.As(err, &multi) {
		t.Fatalf("ResolveTreeDir error = %T (%v), want *MultipleTreeDirsError", err, err)
	}
	want := []string{treeA, treeB}
	if !slices.Equal(multi.Dirs, want) {
		t.Errorf("Dirs = %v, want %v", multi.Dirs, want)
	}
}
