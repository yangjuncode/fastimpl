package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/issuecontext"
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

// 运行管线最终返回的错误也必须进入传入的 stderr 日志流。GUI 的日志页
// 与 log_file 都只消费这个流；若只在状态栏保存 err，失败后日志会停在
// 最后一个正常阶段，看不到真正的失败原因。
func TestRunLogsReturnedErrorToRunLog(t *testing.T) {
	work := t.TempDir()
	var stderr strings.Builder
	err := RunObserved(context.Background(), Options{WorkDir: work, SpecIID: 8}, io.Discard, &stderr, Observer{})
	if err == nil {
		t.Fatal("RunObserved succeeded outside a repository; test setup is wrong")
	}
	logText := stderr.String()
	if !strings.Contains(logText, "level=ERROR") || !strings.Contains(logText, "run failed") {
		t.Fatalf("run log = %q, want an ERROR run failed record", logText)
	}
	if !strings.Contains(logText, err.Error()) {
		t.Fatalf("run log = %q, want the returned error %q", logText, err.Error())
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

// unexpectedForge 只用于把运行管线推进到调度门禁；正确的门禁后路径
// 在配置重载失败时不会进入任何 forge 写操作。
type unexpectedForge struct{}

func (unexpectedForge) IssueState(context.Context, int, int) (forge.IssueInfo, error) {
	return forge.IssueInfo{}, errors.New("unexpected forge use")
}
func (unexpectedForge) IssueComments(context.Context, int, int) ([]forge.IssueComment, error) {
	return nil, errors.New("unexpected forge use")
}
func (unexpectedForge) Claim(context.Context, int, int) error {
	return errors.New("unexpected forge use")
}
func (unexpectedForge) MarkInProgress(context.Context, int, int) error {
	return errors.New("unexpected forge use")
}
func (unexpectedForge) MarkDone(context.Context, int, int) error {
	return errors.New("unexpected forge use")
}
func (unexpectedForge) Close(context.Context, int, int) error {
	return errors.New("unexpected forge use")
}
func (unexpectedForge) Note(context.Context, int, int, string) error {
	return errors.New("unexpected forge use")
}
func (unexpectedForge) EnsureMR(context.Context, int, string, string, string, string) (string, error) {
	return "", errors.New("unexpected forge use")
}

// 门禁等待期间保存的新配置必须在真正调度前重新生效。这里在
// BeforeSchedule 中把合法配置改成非法配置：若管线使用门禁前缓存的
// Config，它会继续走到 prompt.Load 并报告缺失模板；若正确重载，它会
// 先报告新配置里的 parallelism 校验错误。
func TestRunObservedReloadsConfigAfterScheduleGate(t *testing.T) {
	repoDir := gitRepo(t)
	work := t.TempDir()
	configPath := filepath.Join(work, config.DefaultFile)
	if err := os.WriteFile(configPath, []byte("parallelism = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldBuildGraph := buildGraphFn
	defer func() { buildGraphFn = oldBuildGraph }()
	buildGraphFn = func(ctx context.Context, kind, dir, host, projectPath string, specIID int, stderr io.Writer, log *slog.Logger, progress func(graph.BuildProgress), prefetch bool) (forge.Forge, *graph.Graph, *issuecontext.Store, error) {
		g := graph.New(graph.Key{ProjectID: 1, IID: specIID})
		return unexpectedForge{}, g, nil, nil
	}

	missingTemplate := filepath.Join(work, "missing.prompt")
	err := RunObserved(context.Background(), Options{
		WorkDir:      work,
		RepoDir:      repoDir,
		SpecIID:      8,
		TemplatePath: missingTemplate,
	}, io.Discard, io.Discard, Observer{
		BeforeSchedule: func(context.Context) error {
			return os.WriteFile(configPath, []byte("parallelism = 0\n"), 0o644)
		},
	})
	if err == nil {
		t.Fatal("RunObserved succeeded with an invalid config written during the schedule gate")
	}
	if !strings.Contains(err.Error(), "parallelism must be at least 1") {
		t.Fatalf("err = %q, want the reloaded config validation error; the stale pre-gate config was used", err)
	}
}

// runtimeSettingsLoader 是 GUI 保存与已经开始的调度之间的桥：每次调用
// 都必须重读 work-dir 里的配置与提示词，不能只转发 New 时的静态快照。
func TestRuntimeSettingsLoaderReadsLatestWorkDirFiles(t *testing.T) {
	work := t.TempDir()
	write := func(configText, promptText string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, config.DefaultFile), []byte(configText), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "fastimpl.prompt.md"), []byte(promptText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("parallelism = 2\nretries = 1\nagent_cmd = 'old-agent --safe'\nrequire_review = false\nreviewer = 'gpt-reviewer'\nterminal_retention = '0s'\n", "old prompt {{.IssueNo}}\n")
	loader := runtimeSettingsLoader(work, Options{WorkDir: work, SpecIID: 8})
	first, err := loader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(first.AgentArgv, " "); got != "old-agent --safe" {
		t.Fatalf("first agent argv = %q", got)
	}
	if first.Template != "old prompt {{.IssueNo}}\n" || first.RequireReview {
		t.Fatalf("first settings = %+v, want the initial files", first)
	}

	write("parallelism = 3\nretries = 4\nagent_cmd = 'new-agent --latest'\nrequire_review = true\nreviewer = 'swe-reviewer'\nterminal_retention = 'never'\n", "new prompt {{.Reviewer}}\n")
	second, err := loader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(second.AgentArgv, " "); got != "new-agent --latest" {
		t.Fatalf("second agent argv = %q, want the latest saved config", got)
	}
	if second.Parallelism != 3 || second.Retries != 4 || !second.RequireReview || second.Reviewer != "swe-reviewer" || second.TerminalRetention != "never" {
		t.Fatalf("second settings = %+v, want the latest saved config", second)
	}
	if second.Template != "new prompt {{.Reviewer}}\n" {
		t.Fatalf("second template = %q, want the latest saved prompt", second.Template)
	}
}
