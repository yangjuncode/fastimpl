package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
