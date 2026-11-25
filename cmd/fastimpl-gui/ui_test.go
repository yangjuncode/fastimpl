package main

import (
	"context"
	"testing"
	"time"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

func TestMain(m *testing.M) { nsgotest.Main(m) }

// headless 起一个无窗口的应用，返回它、模型与会话。目录预览换成固定的
// 结果，测试因此不碰真实 git 仓库。
func headless(t *testing.T, opts app.Options) (*nsgotest.App, *model, *session) {
	t.Helper()
	return headlessWith(t, opts, fakePreview)
}

// headlessWith 同 headless，但用调用方给的目录预览实现。
func headlessWith(t *testing.T, opts app.Options, preview func(context.Context, app.Options) (app.PathsPreview, error)) (*nsgotest.App, *model, *session) {
	t.Helper()
	var m *model
	var s *session
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		s = newSession()
		s.previewFn = preview
		m = newModel(cx, s, opts)
		return m.build()
	}, nsgotest.Idiom(core.IdiomDesktop), nsgotest.Size(1180, 760))
	return app, m, s
}

// fakePreview 是三个目录都解析成功的假预览。
func fakePreview(context.Context, app.Options) (app.PathsPreview, error) {
	return app.PathsPreview{Paths: app.Paths{WorkDir: "/w", RepoDir: "/w/repo", TreeDir: "/w/repo/.worktrees"}}, nil
}

// loadGraph 把一份快照直接塞进模型，跳过真实构图。
func loadGraph(app *nsgotest.App, m *model, snap snapshot) {
	snap.GraphReady = true
	app.Post(func() {
		nodes := make(map[graph.Key]graph.Node, len(snap.Nodes))
		runtime := make(map[graph.Key]scheduler.TaskRuntime, len(snap.Status))
		for _, n := range snap.Nodes {
			nodes[n.Key] = n
		}
		for _, rt := range snap.Status {
			runtime[rt.Key] = rt
		}
		m.mu.Lock()
		ready := make(map[graph.Key]bool, len(snap.Ready))
		for _, key := range snap.Ready {
			ready[key] = true
		}
		m.snap, m.nodes, m.runtime, m.ready = snap, nodes, runtime, ready
		m.structure = buildDiagram(snap)
		m.flowReady = true
		m.mu.Unlock()
		m.terms.sync(snap.Status)
		m.rev.Update(func(n int) int { return n + 1 })
	})
	app.Settle()
}

// 没传 issue 时主界面是启动表单：issue 编号与三个目录都在，居中显示。
// 目录自动解析成功后会预填，并在底部只读信息栏显示。
func TestEmptyStateRenders(t *testing.T) {
	app, m, _ := headless(t, app.Options{})
	waitPaths(t, app, m)
	app.Find("id:version")
	app.Find("id:empty")
	app.Find("id:empty-panel")
	app.Find("id:form-issue")
	app.Find("id:form-work")
	app.Find("id:form-repo")
	app.Find("id:form-tree")
	app.Find("id:empty-start")
	app.Find("id:paths-bar")
	app.Find("id:path-工作目录")
	app.Find("id:path-仓库目录")
	app.Find("id:path-Work Tree 目录")
}

// waitPaths 等目录预览完成并刷新到界面。测试里的预览是同步返回的，
// 所以只需要让引擎把排队的通知跑完。"完成"包括解析失败：失败也是一种
// 终态，界面这时停在表单里等用户补填。
func waitPaths(t *testing.T, app *nsgotest.App, m *model) {
	t.Helper()
	settled := func() bool {
		s := m.session.Snapshot()
		return s.PathsReady || s.RepoPathErr != nil || s.TreePathErr != nil || s.PathErr != nil
	}
	for range 20 {
		if settled() {
			app.Settle()
			app.Post(func() {})
			app.Settle()
			return
		}
		app.Settle()
	}
	t.Fatal("the directory preview never finished")
}

// 空状态里录入非法编号：留在空状态并给出提示，不崩溃。
func TestEmptyStateRejectsBadIssue(t *testing.T) {
	app, m, _ := headless(t, app.Options{})
	m.issueText.Set("abc")
	app.Press("id:empty-start")
	if snap := m.session.Snapshot(); snap.Phase != phaseFailed {
		t.Fatalf("phase = %v, want failed for a bad issue number", snap.Phase)
	}
	app.Find("id:empty")
}

// 有图之后首页画出节点卡片与图例。
func TestGraphPageRendersNodes(t *testing.T) {
	app, m, _ := headless(t, app.Options{})
	loadGraph(app, m, specSnapshot())
	for _, want := range []string{"spec title", "task A", "task B", "cross repo blocker"} {
		app.Find("text:" + want)
	}
	app.Find("text:可开始工作")
	app.Find("id:legend")
	app.Find("id:graph")
}

// 日志 tab 是独立的运行日志页：从图页切过去后能看到快照里的实时日志。
func TestLogTabRendersRunLog(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	snap.Log = []string{
		"time=2026-01-01T00:00:00Z level=INFO msg=\"worker started\" key=1!9",
		"time=2026-01-01T00:03:00Z level=INFO msg=\"task integrated\" key=1!9",
	}
	loadGraph(gui, m, snap)
	gui.Find("id:tab-log")
	gui.Press("id:tab-log")
	gui.Find("id:log-page")
	gui.Find("id:log-scroll")
	gui.Find("text:worker started")
	gui.Find("text:task integrated")
}

// 终端 tab 用 issue 号与开始时间命名；tooltip 展示 issue、标题、
// 工作时长与 hc 连接信息，并随界面时钟每秒更新。
func TestTerminalTabShowsStartTimeAndTooltip(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	loadGraph(gui, m, snap)
	wantTitle := "#9 · " + started.Format("15:04:05")
	gui.Find("text:" + wantTitle)

	gui.Hover("id:terminal-tab-1!9")
	gui.Advance(600 * time.Millisecond)
	for _, want := range []string{
		"Issue：#9", "标题：task A", "开始时间：", "已工作时间：0秒",
		"hc 终端：proj-issue-9", "hc 地址：t1", "hc attach proj-issue-9",
	} {
		gui.Find("text:" + want)
	}
	// 启动本身会消耗几十毫秒模拟帧时间，因此等到第三次秒 tick 后
	// 截断显示的时长才稳定落在 2 秒。
	gui.Advance(3 * time.Second)
	gui.Find("text:已工作时间：2秒")
}

// 已完成的任务不再随界面时钟增长：tooltip 里的时长固定为完成时刻减开始时刻。
func TestTerminalTabFinishedElapsedIsFixed(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	snap.Status[0].FinishedAt = started.Add(3*time.Minute + 2*time.Second)
	loadGraph(gui, m, snap)
	gui.Hover("id:terminal-tab-1!9")
	gui.Advance(600 * time.Millisecond)
	gui.Find("text:已工作时间：3分2秒")
	gui.Advance(3 * time.Second)
	gui.Find("text:已工作时间：3分2秒")
	if gui.Exists(`text:已工作时间：3分3秒`) {
		t.Fatal("the elapsed time kept ticking after the task finished")
	}
}

// 集成分支终端也用 spec issue 的开始时间命名，但带“集成”前缀；
// tooltip 明确它不是普通任务 worker。
func TestIntegrationTerminalTabUsesIntegrationPrefix(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status = append(snap.Status, scheduler.TaskRuntime{
		Key:         snap.Spec.Key,
		Addr:        "integration-addr",
		Label:       "proj-issue-8-integration",
		Integration: true,
		StartedAt:   started,
	})
	loadGraph(gui, m, snap)
	wantTitle := "集成 #8 · " + started.Format("15:04:05")
	gui.Find("text:" + wantTitle)
	gui.Hover("id:terminal-tab-integration-1!8")
	gui.Advance(600 * time.Millisecond)
	for _, want := range []string{"类型：集成分支 agent", "Issue：#8", "标题：spec title", "hc 终端：proj-issue-8-integration"} {
		gui.Find("text:" + want)
	}
}

// 传了 issue 时窗口打开后自动开始：这里只验证录入值被带到选项里，
// 真正的运行由 session 的测试覆盖。
func TestIssueFromFlagsPrefills(t *testing.T) {
	app, m, _ := headless(t, app.Options{SpecIID: 12})
	if got := m.issueText.GetUntracked(); got != "12" {
		t.Fatalf("issue field = %q, want the flag's issue", got)
	}
	_ = app
}

// 设置对话框：配置 Tab 展示 TOML 参数，提示词 Tab 可切换并展示模板编辑器。
func TestSettingsDialogRendersConfigAndPromptTabs(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	waitPaths(t, gui, m)
	gui.Press("id:open-settings")
	gui.Find("id:settings")
	gui.Find("id:settings-parallelism")
	gui.Find("id:settings-retries")
	gui.Find("id:settings-log-file")
	gui.Find("id:settings-forge")
	gui.Find("id:settings-repo-dir")
	gui.Find("id:settings-tree-dir")
	gui.Find("id:settings-agent-cmd")
	gui.Find("id:settings-integration")
	gui.Find("id:settings-require-review")
	gui.Find("id:settings-hc-binary")
	gui.Find("id:settings-hc-socket")
	gui.Find("id:settings-instance-id")
	gui.Press("id:settings-tab-prompt")
	gui.Find("id:settings-prompt")
}
