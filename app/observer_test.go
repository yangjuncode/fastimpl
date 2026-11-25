package app

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/graph"
)

// gitRepo 建一个临时 git 仓库并返回其顶层目录。
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@test"},
		{"config", "user.name", "test"},
		{"remote", "add", "origin", "https://gitlab.example.com/group/proj.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return dir
}

func TestVerifyRepoRootAcceptsTopLevel(t *testing.T) {
	dir := gitRepo(t)
	if err := verifyRepoRoot(context.Background(), dir); err != nil {
		t.Fatalf("verifyRepoRoot(%s) = %v, want nil", dir, err)
	}
}

// 子目录挂在仓库里，git 会一路找到父仓库的 origin；显式路径必须
// 自己就是顶层，否则用户的目录会被认成另一个项目的 checkout。
func TestVerifyRepoRootRejectsSubdirectory(t *testing.T) {
	root := gitRepo(t)
	sub := filepath.Join(root, "nested", "deeper")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	err := verifyRepoRoot(context.Background(), sub)
	if err == nil {
		t.Fatal("verifyRepoRoot accepted a subdirectory of a repository")
	}
	if !IsRepositoryError(err) {
		t.Fatalf("err = %v (%T), want a *RepositoryError", err, err)
	}
	var repoErr *RepositoryError
	if !errors.As(err, &repoErr) {
		t.Fatal("errors.As did not find the *RepositoryError")
	}
	if repoErr.Dir != sub {
		t.Errorf("RepositoryError.Dir = %q, want %q", repoErr.Dir, sub)
	}
	if !strings.Contains(err.Error(), "top level") {
		t.Errorf("err = %q, want it to name the top-level requirement", err)
	}
	if repoErr.Unwrap() == nil {
		t.Error("RepositoryError.Unwrap() = nil, want the cause")
	}
}

func TestVerifyRepoRootRejectsNonRepository(t *testing.T) {
	dir := t.TempDir()
	err := verifyRepoRoot(context.Background(), dir)
	if err == nil {
		t.Fatal("verifyRepoRoot accepted a directory outside any repository")
	}
	if !IsRepositoryError(err) {
		t.Fatalf("err = %v (%T), want a *RepositoryError", err, err)
	}
}

// Unwrap 保全 CLI 的错误文本：包裹后 Error() 与原因逐字一致。
func TestRepositoryErrorKeepsText(t *testing.T) {
	cause := errors.New("no git repository found; use -repo-dir")
	err := repoError("/some/where", cause)
	if err.Error() != cause.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), cause.Error())
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is did not reach the cause")
	}
	if repoError("", nil) != nil {
		t.Error("repoError(nil) = non-nil, want nil")
	}
}

// 普通配置/forge/API 错误不会被当成仓库错误，GUI 才不至于对它们
// 弹仓库设置。
func TestOrdinaryErrorsAreNotRepositoryErrors(t *testing.T) {
	for _, err := range []error{
		errors.New("decode config fastimpl.toml: bad"),
		errors.New("ensure merge request: 403"),
		errors.New("unknown forge \"nope\""),
	} {
		if IsRepositoryError(err) {
			t.Errorf("IsRepositoryError(%v) = true, want false", err)
		}
	}
}

func TestObserverObserving(t *testing.T) {
	if (Observer{}).observing() {
		t.Error("the zero Observer reports observing; the CLI run must keep its old behavior")
	}
	if !(Observer{Progress: func(graph.BuildProgress) {}}).observing() {
		t.Error("an Observer with Progress does not report observing")
	}
	if !(Observer{ConfigLoaded: func(config.Config) {}}).observing() {
		t.Error("an Observer with ConfigLoaded does not report observing")
	}
}

// ConfigLoaded 必须在仓库解析前送达，GUI 才能从第一条仓库/构图日志
// 开始持久化；即使仓库稍后不可用，回调里的 WorkDir/log_file 也已可用。
func TestRunObservedReportsLoadedConfigBeforeRepoResolution(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "fastimpl.toml"), []byte("log_file = 'run.log'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got config.Config
	err := RunObserved(context.Background(), Options{WorkDir: work, SpecIID: 8}, io.Discard, io.Discard, Observer{
		ConfigLoaded: func(cfg config.Config) { got = cfg },
	})
	if err == nil {
		t.Fatal("RunObserved succeeded outside a repository; test setup is wrong")
	}
	if got.WorkDir != work || got.LogFile != "run.log" || got.SpecIID != 8 {
		t.Fatalf("ConfigLoaded = %+v, want the effective workdir/log_file/spec", got)
	}
}
