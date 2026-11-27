package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// testGraph 建一个两个节点的小图。
func testGraph() *graph.Graph {
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	return g
}

// testScheduler 组装一个只用于快照的调度器：Status 只读图与运行数据，
// 不做任何 IO。
func testScheduler(g *graph.Graph) *scheduler.Scheduler {
	return scheduler.New(config.Config{}, nil, nil, "gitlab", g, nil, "", "",
		"", "issue-8-integration", "main", nil, slog.Default())
}

// startSession 启动一次假运行：runFn 建好图与调度器后立刻返回，模拟
// 一次没有真实 IO 的顺利运行。
func startSession(t *testing.T) (*session, chan struct{}) {
	t.Helper()
	s := newSession()
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		g := testGraph()
		if obs.Progress != nil {
			obs.Progress(graph.BuildProgress{Phase: graph.PhaseDone, Discovered: 1, Read: 1, Open: 1})
		}
		if obs.GraphReady != nil {
			obs.GraphReady(g)
		}
		if obs.SchedulerReady != nil {
			obs.SchedulerReady(testScheduler(g), nil)
		}
		return nil
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatalf("Start = %v", err)
	}
	return s, changes
}

// waitDone 等运行结束的通知，用短上限保证回归时快速失败而不是挂住。
func waitDone(t *testing.T, s *session, changes chan struct{}) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if !s.Running() {
			return
		}
		select {
		case <-changes:
		case <-deadline:
			t.Fatal("the run never reported its end: the completion notification did not arrive")
		}
	}
}

// 回归：结束序列曾在持有 session 锁时调用 refresh，refresh 再取同一把
// 锁，于是每次运行返回后永久死锁——界面永远看不到完成。现在结束通知
// 必须抵达，Running 必须转假，最终快照必须留着节点与状态。
func TestSessionRunCompletionNotificationArrives(t *testing.T) {
	s, changes := startSession(t)
	waitDone(t, s, changes)

	snap := s.Snapshot()
	if snap.Phase != phaseDone {
		t.Fatalf("phase = %v, want done", snap.Phase)
	}
	if snap.Running {
		t.Error("Running() = true after the run returned")
	}
	if !snap.GraphReady {
		t.Error("the final snapshot lost the graph")
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].Key != (graph.Key{ProjectID: 1, IID: 9}) {
		t.Fatalf("final nodes = %+v, want the task node kept", snap.Nodes)
	}
	if len(snap.Status) != 1 || snap.Status[0].Key != (graph.Key{ProjectID: 1, IID: 9}) {
		t.Fatalf("final status = %+v, want one entry per node", snap.Status)
	}
	if snap.Message == "" {
		t.Error("the final snapshot has no message for the status line")
	}
	// 结束之后仍能再一次启动：running 已经被正确放掉，而不是永远卡住。
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatalf("Start after the run ended = %v, want it accepted", err)
	}
}

// 结束序列在刷新最终快照期间必须仍然拒绝新启动：否则新 run 会覆盖
// 还没落定的最终状态。
func TestSessionRunRejectsStartDuringFinalizing(t *testing.T) {
	s := newSession()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the fake run never started")
	}
	if err := s.Start(app.Options{SpecIID: 9}); err == nil {
		t.Fatal("Start accepted a second run while the first was still running")
	}
	close(release)
	deadline := time.After(3 * time.Second)
	for s.Running() {
		select {
		case <-deadline:
			t.Fatal("the run never finished")
		case <-time.After(time.Millisecond):
		}
	}
	if err := s.Start(app.Options{SpecIID: 9}); err != nil {
		t.Fatalf("Start after the run ended = %v, want it accepted", err)
	}
}

// 失败路径同样要走完结束序列：错误进快照，运行转假，通知抵达。
func TestSessionRunFailureIsReported(t *testing.T) {
	s := newSession()
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		return errors.New("forge refused")
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, changes)
	snap := s.Snapshot()
	if snap.Phase != phaseFailed {
		t.Fatalf("phase = %v, want failed", snap.Phase)
	}
	if snap.Err == nil {
		t.Error("the final snapshot has no error")
	}
	if snap.Running {
		t.Error("Running() = true after a failed run returned")
	}
}

// 仓库错误必须在快照里可识别，界面才能弹设置。
func TestSessionRunRepositoryErrorIsClassified(t *testing.T) {
	s := newSession()
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		return &app.RepositoryError{}
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, changes)
	snap := s.Snapshot()
	if snap.RepoError == nil {
		t.Fatal("the snapshot lost the repository error: the settings dialog cannot open")
	}
	if snap.Phase != phaseFailed {
		t.Fatalf("phase = %v, want failed", snap.Phase)
	}
	if !app.IsRepositoryError(snap.Err) {
		t.Fatalf("Err = %v, want it to stay a repository error", snap.Err)
	}
}

// 取消不是成功：结束后状态必须是"已停止"。
func TestSessionRunCancelIsNotSuccess(t *testing.T) {
	s := newSession()
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	waitDone(t, s, changes)
	snap := s.Snapshot()
	if snap.Phase != phaseStopped {
		t.Fatalf("phase = %v, want stopped after a cancel", snap.Phase)
	}
	if snap.Message == "" || snap.Message == "全部任务已集成" {
		t.Fatalf("message = %q, want it to say the run was stopped", snap.Message)
	}
}

// 日志必须在运行期间进入快照，而不是等 run 返回后才一次性出现。
func TestSessionLogStreamsWhileRunIsActive(t *testing.T) {
	s := newSession()
	changes := make(chan struct{}, 32)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	release := make(chan struct{})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		g := testGraph()
		if obs.SchedulerReady != nil {
			obs.SchedulerReady(testScheduler(g), nil)
		}
		_, _ = io.WriteString(w, "time=2026-11-25T17:20:03+08:00 level=INFO msg=\"worker started\" key=1!9\n")
		<-release
		return nil
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	snap := waitCondition(t, s, "the live log line", func(snap snapshot) bool {
		for _, line := range snap.Log {
			if strings.Contains(line, "worker started") {
				return true
			}
		}
		return false
	})
	if !snap.Running {
		t.Fatal("the log line appeared only after the run ended")
	}
	close(release)
	waitDone(t, s, changes)
}

// pump 不能只服务 scheduler：配置/仓库/建图阶段的早期日志也必须实时可见。
func TestSessionLogStreamsBeforeSchedulerIsReady(t *testing.T) {
	s := newSession()
	releaseGraph := make(chan struct{})
	releaseRun := make(chan struct{})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		_, _ = io.WriteString(w, "time=2026-11-25T17:20:03+08:00 level=INFO msg=\"building graph\"\n")
		<-releaseGraph
		g := testGraph()
		if obs.SchedulerReady != nil {
			obs.SchedulerReady(testScheduler(g), nil)
		}
		<-releaseRun
		return nil
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	snap := waitCondition(t, s, "the early graph-building log line", func(snap snapshot) bool {
		for _, line := range snap.Log {
			if strings.Contains(line, "building graph") {
				return true
			}
		}
		return false
	})
	if !snap.Running {
		t.Fatal("the early log appeared only after the run ended")
	}
	close(releaseGraph)
	close(releaseRun)
}

// 配置 log_file 后：先限量读入既有文件尾部，再把本次运行日志 append 到同一文件。
func TestSessionLogFileTailIsLoadedAndAppended(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	var existing strings.Builder
	for i := range 230 {
		fmt.Fprintf(&existing, "old-%03d\n", i)
	}
	if err := os.WriteFile(path, []byte(existing.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSession()
	changes := make(chan struct{}, 32)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	release := make(chan struct{})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		if obs.ConfigLoaded == nil {
			return errors.New("the GUI observer lost ConfigLoaded")
		}
		obs.ConfigLoaded(config.Config{WorkDir: dir, LogFile: "run.log"})
		g := testGraph()
		if obs.SchedulerReady != nil {
			obs.SchedulerReady(testScheduler(g), nil)
		}
		_, _ = io.WriteString(w, "time=2026-11-25T17:20:04+08:00 level=INFO msg=\"new line\"\n")
		<-release
		return nil
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	snap := waitCondition(t, s, "the appended log line", func(snap snapshot) bool {
		for _, line := range snap.Log {
			if strings.Contains(line, "new line") {
				return true
			}
		}
		return false
	})
	joined := strings.Join(snap.Log, "\n")
	if strings.Contains(joined, "old-000") {
		t.Fatalf("the snapshot read too much of the existing log; first lines = %.200s", joined)
	}
	if !strings.Contains(joined, "old-229") {
		t.Fatalf("the snapshot lost the tail of the existing log; lines = %v", snap.Log)
	}
	close(release)
	waitDone(t, s, changes)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "old-000\n") {
		t.Fatal("the existing log file was rewritten instead of appended")
	}
	if !strings.HasSuffix(text, "new line\"\n") {
		t.Fatalf("the live line was not appended to the log file; tail = %q", text[max(0, len(text)-160):])
	}
	if got := len(strings.Split(strings.TrimSuffix(text, "\n"), "\n")); got != 231 {
		t.Fatalf("log file lines = %d, want 230 existing + 1 appended", got)
	}
}

// 大文件只按字节窗口读尾部；窗口第一行若是半行必须丢弃，避免把截断内容当完整日志。
func TestReadLogTailUsesByteWindowAndDropsPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.log")
	line := strings.Repeat("x", 100)
	var content strings.Builder
	for range 800 {
		content.WriteString(line)
		content.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := readLogTail(path, logLines, logFileReadBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != logLines {
		t.Fatalf("tail lines = %d, want the capped %d", len(lines), logLines)
	}
	for i, got := range lines {
		if got != line {
			t.Fatalf("line %d is incomplete: len=%d, want %d", i, len(got), len(line))
		}
	}
}

// discoverSession 建一个会话，换上给定的目录预览实现。
func discoverSession(fn func(context.Context, app.Options) (app.PathsPreview, error)) (*session, chan struct{}) {
	s := newSession()
	s.previewFn = fn
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	return s, changes
}

// waitDiscover 等目录预览落到快照里。
func waitDiscover(t *testing.T, s *session, changes chan struct{}) snapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if snap := s.Snapshot(); snap.PathsReady || snap.RepoPathErr != nil || snap.TreePathErr != nil || snap.PathErr != nil {
			return snap
		}
		select {
		case <-changes:
		case <-deadline:
			return s.Snapshot()
		}
	}
}

// 全部目录都能自动解析：三项都填好，Ready 为真。
func TestDiscoverResolvesAllPaths(t *testing.T) {
	s, changes := discoverSession(func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: "/w", RepoDir: "/w/repo", TreeDir: "/w/repo/.worktrees"}}, nil
	})
	s.Discover(app.Options{})
	snap := waitDiscover(t, s, changes)
	if !snap.PathsReady {
		t.Fatalf("PathsReady = false in %+v, want all three resolved", snap.Paths)
	}
	if snap.Paths.WorkDir != "/w" || snap.Paths.RepoDir != "/w/repo" || snap.Paths.TreeDir != "/w/repo/.worktrees" {
		t.Fatalf("paths = %+v, want the previewed three", snap.Paths)
	}
}

// 仓库目录失败：工作目录保留、只在 RepoPathErr 里报错，TreePathErr 为空。
func TestDiscoverKeepsWorkDirWhenRepoFails(t *testing.T) {
	s, changes := discoverSession(func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: "/w"}, RepoErr: errors.New("no git repository")}, nil
	})
	s.Discover(app.Options{})
	snap := waitDiscover(t, s, changes)
	if snap.Paths.WorkDir != "/w" {
		t.Fatalf("WorkDir = %q, want it kept", snap.Paths.WorkDir)
	}
	if snap.RepoPathErr == nil || !strings.Contains(snap.RepoPathErr.Error(), "no git repository") {
		t.Fatalf("RepoPathErr = %v, want the repository failure reported on its own", snap.RepoPathErr)
	}
	if snap.TreePathErr != nil {
		t.Error("TreePathErr set while the repository is unknown")
	}
	if snap.PathsReady {
		t.Error("PathsReady = true with a missing repository")
	}
}

// Work Tree 目录失败：工作目录与仓库目录保留，只在 TreePathErr 里报错。
func TestDiscoverReportsTreeFailure(t *testing.T) {
	s, changes := discoverSession(func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: "/w", RepoDir: "/w/repo"}, TreeErr: errors.New("ambiguous parent")}, nil
	})
	s.Discover(app.Options{})
	snap := waitDiscover(t, s, changes)
	if snap.Paths.RepoDir != "/w/repo" {
		t.Fatalf("RepoDir = %q, want it kept", snap.Paths.RepoDir)
	}
	if snap.TreePathErr == nil || !strings.Contains(snap.TreePathErr.Error(), "ambiguous parent") {
		t.Fatalf("TreePathErr = %v, want the work-tree failure reported on its own", snap.TreePathErr)
	}
	if snap.RepoPathErr != nil {
		t.Error("RepoPathErr set while only the work-tree dir failed")
	}
}

// 工作目录/配置本身失败是普通失败：PathErr 与仓库错误分开。
func TestDiscoverReportsConfigError(t *testing.T) {
	s, changes := discoverSession(func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{}, errors.New("decode config")
	})
	s.Discover(app.Options{})
	snap := waitDiscover(t, s, changes)
	if snap.PathErr == nil {
		t.Fatal("PathErr = nil, want the config failure")
	}
	if snap.RepoPathErr != nil || snap.TreePathErr != nil {
		t.Error("a config failure must not be reported as a repository failure")
	}
}

// 开始运行用的正是界面上三个目录的值（本次运行覆盖，不写配置）。
func TestStartPassesAllThreePaths(t *testing.T) {
	var got app.Options
	gui, m, sess := headless(t, app.Options{})
	sess.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		got = opts
		return nil
	}
	m.workText.Set("/w")
	m.repoText.Set("/w/repo")
	m.treeText.Set("/w/tree")
	m.issueText.Set("7")
	gui.Post(m.start)
	gui.Settle()
	for i := 0; i < 20 && got.SpecIID == 0; i++ {
		gui.Settle()
	}
	if got.SpecIID != 7 || got.WorkDir != "/w" || got.RepoDir != "/w/repo" || got.TreeDir != "/w/tree" {
		t.Fatalf("options = %+v, want the form's issue and three paths", got)
	}
}

// 传了 issue 但目录没解析齐：不得自动开始，停在表单等用户补齐。
func TestAutoStartWaitsForPaths(t *testing.T) {
	gui, m, sess := headlessWith(t, app.Options{SpecIID: 12}, func(context.Context, app.Options) (app.PathsPreview, error) {
		// 仓库目录解析不出来：路径不齐，自动开始必须被挡住。
		return app.PathsPreview{Paths: app.Paths{WorkDir: "/w"}, RepoErr: errors.New("no git repository")}, nil
	})
	waitPaths(t, gui, m)
	if sess.Snapshot().AutoStarted {
		t.Fatal("the run auto-started although the paths were not resolved")
	}
	snap := sess.Snapshot()
	if snap.Running || snap.Phase != phaseIdle {
		t.Fatalf("snapshot = %+v, want the idle form", snap)
	}
	if m.issueText.GetUntracked() != "12" {
		t.Fatalf("issue = %q, want the passed issue preserved", m.issueText.GetUntracked())
	}
}

// waitCondition 轮询会话快照直到条件满足；只用于短小的本地异步测试。
func waitCondition(t *testing.T, s *session, what string, cond func(snapshot) bool) snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		if cond(snap) {
			return snap
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last snapshot = %+v", what, s.Snapshot())
	return snapshot{}
}

// 调度门禁：依赖图建好后运行必须停在 ready，BeforeSchedule 不得自行放行；
// 只有用户点击开始后 SchedulerReady 才会到来，hcgo/worker 也因此不会提前启动。
func TestSessionWaitsAtScheduleGateUntilConfirmed(t *testing.T) {
	s := newSession()
	changes := make(chan struct{}, 32)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	released := make(chan struct{})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		g := testGraph()
		if obs.GraphReady != nil {
			obs.GraphReady(g)
		}
		if obs.BeforeSchedule == nil {
			return errors.New("the GUI observer lost BeforeSchedule")
		}
		if err := obs.BeforeSchedule(ctx); err != nil {
			return err
		}
		close(released)
		if obs.SchedulerReady != nil {
			obs.SchedulerReady(testScheduler(g), nil)
		}
		return nil
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	snap := waitCondition(t, s, "the schedule gate", func(snap snapshot) bool { return snap.WaitingStart })
	if snap.Phase != phaseReady {
		t.Fatalf("phase = %v, want ready while waiting at the gate", snap.Phase)
	}
	if !snap.GraphReady {
		t.Error("GraphReady = false at the gate; the graph must already be renderable")
	}
	wantReady := graph.Key{ProjectID: 1, IID: 9}
	if len(snap.Ready) != 1 || snap.Ready[0] != wantReady {
		t.Fatalf("Ready = %+v, want only %+v", snap.Ready, wantReady)
	}
	select {
	case <-released:
		t.Fatal("BeforeSchedule released before the user pressed start")
	default:
	}

	s.StartScheduling()
	waitDone(t, s, changes)
	select {
	case <-released:
	default:
		t.Fatal("BeforeSchedule did not release after StartScheduling")
	}
	snap = s.Snapshot()
	if snap.Phase != phaseDone {
		t.Fatalf("phase = %v, want done after the fake run finishes", snap.Phase)
	}
	if snap.WaitingStart {
		t.Error("WaitingStart survived the end of the run")
	}
}

// 在门禁前停止：等待中的 BeforeSchedule 必须随 ctx 退出，最终状态是 stopped，
// 不能让运行 goroutine 永远卡在 ready。
func TestSessionStopReleasesScheduleGate(t *testing.T) {
	s := newSession()
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		if obs.GraphReady != nil {
			obs.GraphReady(testGraph())
		}
		return obs.BeforeSchedule(ctx)
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	waitCondition(t, s, "the schedule gate", func(snap snapshot) bool { return snap.WaitingStart })
	s.Stop()
	waitDone(t, s, changes)
	if snap := s.Snapshot(); snap.Phase != phaseStopped {
		t.Fatalf("phase = %v, want stopped after canceling at the gate", snap.Phase)
	}
}

// 显式输入 issue 也不能绕过只读观察：app preflight 发现 live supervisor 后，
// GUI 应切到 observer，而不是先把这次运行标成失败或继续准备集成分支。
func TestSessionRunActiveConflictSwitchesToObserving(t *testing.T) {
	meta := guiActiveMeta(t.TempDir(), 8, 9, os.Getpid(), time.Now())
	s := newSession()
	s.runFn = func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		return &app.ActiveRunConflictError{SpecIID: 8, Tasks: []scheduler.ActiveTaskMeta{meta}}
	}
	if err := s.Start(app.Options{SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	snap := waitCondition(t, s, "observer mode after an active run conflict", func(snap snapshot) bool { return snap.Observing })
	if snap.Phase != phaseObserving || snap.Err != nil {
		t.Fatalf("snapshot = %+v, want clean read-only observation", snap)
	}
	if len(snap.Status) != 1 || snap.Status[0].Addr != "t-active" {
		t.Fatalf("status = %+v, want the observed terminal", snap.Status)
	}
}

// guiActiveMeta 构造一份 active metadata。scheduler 的 stage 类型故意不导出，
// 但它是字符串类型，测试可以用可赋值的字符串常量填写 JSON 字段。
func guiActiveMeta(repoDir string, spec, issue int, pid int, heartbeat time.Time) scheduler.ActiveTaskMeta {
	return scheduler.ActiveTaskMeta{
		Version:           1,
		RepoDir:           repoDir,
		WorkDir:           repoDir + "/work",
		TreeDir:           repoDir + "/.worktrees",
		Worktree:          repoDir + "/.worktrees/issue-" + strconv.Itoa(issue),
		IntegrationDir:    repoDir + "/.worktrees/issue-" + strconv.Itoa(spec) + "-integration",
		ProjectID:         1,
		SpecIID:           spec,
		IssueIID:          issue,
		Branch:            "issue-" + strconv.Itoa(issue),
		IntegrationBranch: "issue-" + strconv.Itoa(spec) + "-integration",
		TerminalAddr:      "t-active",
		TerminalName:      "repo-issue-" + strconv.Itoa(issue),
		HCBinary:          "hc",
		ClaimToken:        "claim",
		ReportToken:       "report",
		Stage:             "waiting_report",
		StartedAt:         heartbeat.Add(-time.Minute),
		UpdatedAt:         heartbeat,
		Supervisor: scheduler.SupervisorMeta{
			PID:         pid,
			InstanceID:  "test-box-1",
			HeartbeatAt: heartbeat,
		},
	}
}

func writeGUIActiveMeta(t *testing.T, meta scheduler.ActiveTaskMeta) {
	t.Helper()
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scheduler.ActiveMetaPath(meta.RepoDir, meta.IssueIID), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 目录预览完成后必须扫描本仓库 active metadata；这让空 issue 启动的 GUI
// 能发现上次关闭时仍在运行的 worker。
func TestDiscoverScansActiveRuns(t *testing.T) {
	repoDir := t.TempDir()
	meta := guiActiveMeta(repoDir, 8, 9, -1, time.Now())
	writeGUIActiveMeta(t, meta)
	s, _ := discoverSession(func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	})
	s.Discover(app.Options{})
	snap := waitCondition(t, s, "active run scan", func(snap snapshot) bool { return len(snap.ActiveRuns) == 1 })
	if snap.ActiveRuns[0].SpecIID != 8 || snap.ActiveRuns[0].IssueIID != 9 || snap.ActiveRuns[0].TerminalAddr != "t-active" {
		t.Fatalf("ActiveRuns = %+v, want the scanned worker", snap.ActiveRuns)
	}
}

// 只读观察模式不是一次调度：running 必须为 false，但状态里要有终端页
// 所需的 addr/label/started，以便用户能看到原 supervisor 正在管理的 hc 终端。
func TestObserveActiveRunPopulatesTerminalSnapshot(t *testing.T) {
	meta := guiActiveMeta(t.TempDir(), 8, 9, os.Getpid(), time.Now())
	s := newSession()
	s.ObserveActive([]scheduler.ActiveTaskMeta{meta})
	snap := s.Snapshot()
	if snap.Phase != phaseObserving || !snap.Observing {
		t.Fatalf("phase/observing = %v/%v, want observing", snap.Phase, snap.Observing)
	}
	if snap.Running {
		t.Fatal("observer mode must not count as a local scheduler run")
	}
	if len(snap.Status) != 1 || snap.Status[0].Addr != "t-active" || snap.Status[0].StartedAt.IsZero() {
		t.Fatalf("status = %+v, want terminal identity and original start time", snap.Status)
	}
	if len(snap.ActiveRuns) != 1 || snap.ActiveRuns[0].Worktree == "" || snap.ActiveRuns[0].Supervisor.PID != os.Getpid() {
		t.Fatalf("ActiveRuns = %+v, want observer metadata retained for the page", snap.ActiveRuns)
	}
	if snap.Issue != 8 {
		t.Fatalf("issue = %d, want the observed spec", snap.Issue)
	}
}

// 并行任务中其中一个完成并删除 metadata，不代表其他 active worker 也消失。
// 只要还有任务的 supervisor 活着，观察者就必须收缩到剩余任务继续只读观察，
// 不能退出成一个既不看也不接管的空状态。
func TestObservedPartialCompletionKeepsWatchingRemainingRuns(t *testing.T) {
	repoDir := t.TempDir()
	meta9 := guiActiveMeta(repoDir, 8, 9, os.Getpid(), time.Now())
	meta10 := guiActiveMeta(repoDir, 8, 10, os.Getpid(), time.Now())
	writeGUIActiveMeta(t, meta9)
	writeGUIActiveMeta(t, meta10)
	s := newSession()
	s.ObserveActive([]scheduler.ActiveTaskMeta{meta9, meta10})
	defer s.StopObserving()
	if err := os.Remove(scheduler.ActiveMetaPath(repoDir, 9)); err != nil {
		t.Fatal(err)
	}
	if s.checkObserved([]scheduler.ActiveTaskMeta{meta9, meta10}) {
		t.Fatal("observer exited when only one of two active tasks disappeared")
	}
	snap := s.Snapshot()
	if !snap.Observing || len(snap.ActiveRuns) != 1 || snap.ActiveRuns[0].IssueIID != 10 {
		t.Fatalf("observing=%v active=%+v, want continued observation of issue 10", snap.Observing, snap.ActiveRuns)
	}
}

// active metadata 没变化时观察轮询不应重复通知 UI。否则缩短轮询或任务较多时，
// 无意义 revision 会持续喂给 Settle/渲染队列，甚至让界面事件循环饿死。
func TestObservedUnchangedMetadataDoesNotNotifyAgain(t *testing.T) {
	repoDir := t.TempDir()
	meta := guiActiveMeta(repoDir, 8, 9, os.Getpid(), time.Now())
	writeGUIActiveMeta(t, meta)
	s := newSession()
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() { changes <- struct{}{} })
	s.ObserveActive([]scheduler.ActiveTaskMeta{meta})
	defer s.StopObserving()
	for len(changes) > 0 {
		<-changes
	}
	if s.checkObserved([]scheduler.ActiveTaskMeta{meta}) {
		t.Fatal("fresh live supervisor unexpectedly ended observation")
	}
	for len(changes) > 0 {
		<-changes
	}
	if s.checkObserved([]scheduler.ActiveTaskMeta{meta}) {
		t.Fatal("fresh live supervisor unexpectedly ended observation on the second poll")
	}
	select {
	case <-changes:
		t.Fatal("unchanged active metadata produced a duplicate UI notification")
	default:
	}
}

// 无法解析/版本不符的 active metadata 不能在 GUI 启动扫描里静默消失；
// 它可能属于一个当前二进制不理解的新版 worker，自动启动必须保守提示。
func TestDiscoverReportsInvalidActiveMetadata(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(scheduler.ActiveMetaPath(repoDir, 9), []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := discoverSession(func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	})
	s.Discover(app.Options{})
	snap := waitCondition(t, s, "invalid active metadata scan error", func(snap snapshot) bool { return snap.ActiveScanErr != nil })
	if len(snap.ActiveRuns) != 0 {
		t.Fatalf("ActiveRuns = %+v, want invalid metadata excluded", snap.ActiveRuns)
	}
}

// 原 supervisor 在观察开始后仍可能派发新的并行任务。观察者是按 spec 观察
// 整组 active metadata，不是只盯启动瞬间已有的 issue；否则后续任务会脱离
// 只读观察，也不会参与原进程退出后的接管评估。
func TestObservedSpecPicksUpTasksAddedAfterObservationStarted(t *testing.T) {
	repoDir := t.TempDir()
	meta9 := guiActiveMeta(repoDir, 8, 9, os.Getpid(), time.Now())
	writeGUIActiveMeta(t, meta9)
	s := newSession()
	s.ObserveActive([]scheduler.ActiveTaskMeta{meta9})
	defer s.StopObserving()
	meta10 := guiActiveMeta(repoDir, 8, 10, os.Getpid(), time.Now())
	writeGUIActiveMeta(t, meta10)
	if s.checkObserved([]scheduler.ActiveTaskMeta{meta9}) {
		t.Fatal("fresh live supervisor unexpectedly ended observation")
	}
	snap := s.Snapshot()
	if len(snap.ActiveRuns) != 2 {
		t.Fatalf("ActiveRuns = %+v, want the later issue-10 metadata included", snap.ActiveRuns)
	}
	if len(snap.Status) != 2 {
		t.Fatalf("Status = %+v, want a read-only terminal entry for the later issue-10 worker", snap.Status)
	}
}

// GUI 日志页/log_file 必须包含运行管线最终返回的错误。这里不替换
// runFn，走真实 app.RunObserved：失败只进状态栏时快照 Log 为空；
// 正确实现会在 run 返回前把完整错误写入同一个 lineWriter。
func TestSessionRunFailureAppearsInLog(t *testing.T) {
	s := newSession()
	changes := make(chan struct{}, 16)
	s.SetOnChange(func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	if err := s.Start(app.Options{WorkDir: t.TempDir(), SpecIID: 8}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, s, changes)
	snap := s.Snapshot()
	if snap.Phase != phaseFailed || snap.Err == nil {
		t.Fatalf("phase = %v err = %v, want a failed run", snap.Phase, snap.Err)
	}
	joined := strings.Join(snap.Log, "\n")
	if !strings.Contains(joined, "level=ERROR") || !strings.Contains(joined, "run failed") {
		t.Fatalf("GUI log = %q, want the final run failure", joined)
	}
	if !strings.Contains(joined, snap.Err.Error()) {
		t.Fatalf("GUI log = %q, want the returned error %q", joined, snap.Err.Error())
	}
}
