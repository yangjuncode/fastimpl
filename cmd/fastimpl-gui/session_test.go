package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
