package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// headlessRecovery 在启动扫描发生前就装好 runFn；否则非常快完成
// 的 Discover 可能在测试替换 runFn 前触发自动接管，跑到真实 app 管线。
func headlessRecovery(t *testing.T, preview func(context.Context, app.Options) (app.PathsPreview, error), runFn func(context.Context, app.Options, io.Writer, app.Observer) error) (*nsgotest.App, *model, *session) {
	t.Helper()
	return headlessRecoveryOpts(t, app.Options{}, preview, runFn)
}

// headlessRecoveryOpts 同 headlessRecovery，但保留调用方传入的启动 spec。
func headlessRecoveryOpts(t *testing.T, opts app.Options, preview func(context.Context, app.Options) (app.PathsPreview, error), runFn func(context.Context, app.Options, io.Writer, app.Observer) error) (*nsgotest.App, *model, *session) {
	t.Helper()
	var m *model
	var s *session
	gui := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		s = newSession()
		s.previewFn = preview
		s.runFn = runFn
		m = newModel(cx, s, opts)
		return m.build()
	}, nsgotest.Idiom(core.IdiomDesktop), nsgotest.Size(1180, 760))
	return gui, m, s
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
		next := buildDiagram(snap)
		rebuild := !sameStructure(m.structure, next)
		m.snap, m.nodes, m.runtime, m.ready = snap, nodes, runtime, ready
		m.structure = next
		m.flowReady = true
		m.mu.Unlock()
		termsChanged := m.terms.sync(snap.Status)
		m.rev.Update(func(n int) int { return n + 1 })
		if termsChanged {
			m.termsRev.Update(func(n int) int { return n + 1 })
		}
		if rebuild {
			m.structureRev.Update(func(n int) int { return n + 1 })
		}
	})
	app.Settle()
}

// pressGraphNode 在依赖图画布上单击一个节点。坐标先由 flow 的图内
// 布局转换成相机视口坐标，确保测试走的是真实指针路径，而不是直接改模型。
func pressGraphNode(t *testing.T, gui *nsgotest.App, m *model, id string) {
	t.Helper()
	bounds, ok := m.flowState.Bounds(id)
	if !ok {
		t.Fatalf("graph node %s is not laid out", id)
	}
	at := m.cam.ToView(core.Point{X: bounds.Origin.X + bounds.Size.Width/2, Y: bounds.Origin.Y + bounds.Size.Height/2})
	gui.PressAt("id:graph", at)
	gui.Settle()
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

// 图结构本身也要响应刷新：新增节点必须出现，同时已有节点内容按 ID
// 保留，不能因为拓扑重排而整卡 remount。
func TestGraphStructureRefreshesNodesInPlace(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	loadGraph(gui, m, snap)
	before := gui.Find("text:task A")

	key := graph.Key{ProjectID: 1, IID: 30}
	snap.Nodes = append(append([]graph.Node(nil), snap.Nodes...), graph.Node{
		Key: key, Title: "task C", Kind: graph.KindChild, State: graph.Running, Deps: []graph.Key{{ProjectID: 1, IID: 9}},
	})
	loadGraph(gui, m, snap)
	gui.Find("text:task C")
	after := gui.Find("text:task A")
	if before.ID != after.ID {
		t.Fatalf("existing node text remounted during a topology refresh (id %d -> %d)", before.ID, after.ID)
	}
}

// 节点卡片在已有运行时间时必须展示开始、结束与用时；完结任务的
// 用时固定为 FinishedAt - StartedAt。
func TestGraphNodeShowsRuntimeTimes(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	started := time.Date(2026, time.January, 2, 15, 4, 5, 0, time.Local)
	finished := started.Add(3*time.Minute + 2*time.Second)
	snap.Status[0].StartedAt = started
	snap.Status[0].FinishedAt = finished
	loadGraph(gui, m, snap)
	gui.Find("text:开始 " + started.Format("01-02 15:04:05") + " · 用时 3分2秒")
	gui.Find("text:结束 " + finished.Format("01-02 15:04:05"))
}

// 运行中的节点用时也要跟着界面时钟走，而不是只显示第一次刷新时的
// 静态秒数。
func TestGraphNodeRunningElapsedTicks(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	loadGraph(gui, m, snap)
	prefix := "text:开始 " + started.Format("01-02 15:04:05") + " · 用时 "
	before := gui.Find(prefix + "0秒")
	gui.Advance(3 * time.Second)
	after := gui.Find(prefix + "2秒")
	if before.ID != after.ID {
		t.Fatalf("elapsed text view was remounted (id %d -> %d); ticking must update text in place, not rebuild the card", before.ID, after.ID)
	}
}

// 节点状态变化必须在原卡片上更新，而不是只在首次挂载时画一张
// 静态快照；这也防止结构未变时重建依赖图、移动相机。
func TestGraphNodeStatusUpdatesInPlace(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	loadGraph(gui, m, specSnapshot())
	gui.Find("text:可开始工作")
	key := graph.Key{ProjectID: 1, IID: 9}
	gui.Post(func() {
		m.mu.Lock()
		node := m.nodes[key]
		node.State = graph.Running
		m.nodes[key] = node
		m.mu.Unlock()
		m.rev.Update(func(n int) int { return n + 1 })
	})
	gui.Settle()
	gui.Find("id:graph >> text:运行中")
}

// 单击选中就必须立刻更新底部节点操作条；只有“需要处理”或“失败”的
// 节点显示它，普通节点（包括截图里常见的已关闭节点）选中时不占这一栏。
func TestGraphSingleClickUpdatesNodeActionsAndNormalNodesHideThem(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	snap.Nodes[0].State = graph.NeedsAttention
	snap.Nodes[1].State = graph.Failed
	loadGraph(gui, m, snap)

	pressGraphNode(t, gui, m, "1!9")
	if got := m.tab.GetUntracked(); got != pageGraph {
		t.Fatalf("single click opened tab %q, want to stay on the graph", got)
	}
	gui.Find("id:node-actions")
	gui.Find("text:节点 1!9：")

	pressGraphNode(t, gui, m, "1!10")
	if got := m.tab.GetUntracked(); got != pageGraph {
		t.Fatalf("single click opened tab %q, want to stay on the graph", got)
	}
	gui.Find("id:node-actions")
	gui.Find("text:节点 1!10：")

	pressGraphNode(t, gui, m, "2!3")
	if got := m.selected.GetUntracked(); got != "2!3" {
		t.Fatalf("selected node = %q, want 2!3", got)
	}
	if gui.Exists("id:node-actions") {
		t.Fatalf("normal closed node still shows node actions\n%s", gui.Dump())
	}
}

// 日志-only 的全局 revision 不能重建图卡片：卡片 memo 发现自己关心的
// 节点/运行数据没变后，Dynamic 不重新挂载任何 view。
func TestGraphNodeDoesNotRebuildForLogOnlyRevision(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	loadGraph(gui, m, specSnapshot())
	before := gui.Find("text:task A")
	gui.Post(func() { m.rev.Update(func(n int) int { return n + 1 }) })
	gui.Settle()
	after := gui.Find("text:task A")
	if before.ID != after.ID {
		t.Fatalf("log-only revision remounted the node card text (id %d -> %d)", before.ID, after.ID)
	}
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

// 日志内容超出 viewport 时必须有一条可见、可拖的滚动条。用户不能
// 只靠滚轮/触摸板猜测位置；滚动条常显，但内容放得下时仍不出现。
func TestLogTabShowsScrollbarWhenLogsOverflow(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	for i := range 120 {
		snap.Log = append(snap.Log, fmt.Sprintf("time=2026-01-01T00:%02d:00Z level=INFO msg=line-%03d", i%60, i))
	}
	loadGraph(gui, m, snap)
	gui.Press("id:tab-log")
	scroll := gui.Find("id:log-scroll")
	if scroll.Content.Height <= scroll.Frame.Size.Height {
		t.Fatalf("log content %v does not overflow frame %v; test setup is ineffective", scroll.Content, scroll.Frame.Size)
	}
	bar := gui.Find("label:运行日志滚动条")
	if bar.Animated.Opacity != 1 {
		t.Fatalf("overflowing log scrollbar opacity = %v, want 1", bar.Animated.Opacity)
	}
	if !scroll.Scroll.HideIndicator {
		t.Fatal("the custom log scrollbar did not replace the native indicator")
	}
}

// 内容放得下时滚动条仍挂载但完全透明、不接收指针；只有真溢出才占
// 用户的视觉注意力。
func TestLogTabHidesScrollbarWhenLogsFit(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	loadGraph(gui, m, specSnapshot())
	gui.Press("id:tab-log")
	scroll := gui.Find("id:log-scroll")
	if scroll.Content.Height > scroll.Frame.Size.Height {
		t.Fatalf("log content %v unexpectedly overflows frame %v; test setup is ineffective", scroll.Content, scroll.Frame.Size)
	}
	if opacity := gui.Find("label:运行日志滚动条").Animated.Opacity; opacity != 0 {
		t.Fatalf("short log scrollbar opacity = %v, want 0", opacity)
	}
}

// 终端 tab 的选项结构只跟随“页集合/地址”变化。StartedAt 这类运行
// 元数据仍由标题自己的 ContentFunc 更新，不能触发整排 tab 选项重算。
func TestTerminalTabsDoNotRebuildForRuntimeOnlyRevision(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	loadGraph(gui, m, snap)
	base := m.termsRev.GetUntracked()

	loadGraph(gui, m, snap)
	if got := m.termsRev.GetUntracked(); got != base {
		t.Fatalf("identical status changed termsRev %d -> %d", base, got)
	}
	snap.Status[0].StartedAt = gui.Clock.WallTime()
	loadGraph(gui, m, snap)
	if got := m.termsRev.GetUntracked(); got != base {
		t.Fatalf("runtime-only status changed termsRev %d -> %d", base, got)
	}
	snap.Status[0].Addr = "t1-new"
	loadGraph(gui, m, snap)
	if got := m.termsRev.GetUntracked(); got == base {
		t.Fatal("terminal address change did not refresh the terminal tab options")
	}
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

// 关闭中的 tooltip 只是隐藏挂载，不能因为它让界面时钟每秒更新所有
// 终端的隐藏文本；只有用户真正打开 tooltip 后才需要实时详情。
func TestTerminalTooltipDoesNotTickWhileClosed(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	loadGraph(gui, m, snap)
	textOf := func() []string {
		views := gui.QueryMounted("text:已工作时间")
		out := make([]string, len(views))
		for i, view := range views {
			out[i] = view.Text
		}
		return out
	}
	before := textOf()
	gui.Advance(3 * time.Second)
	after := textOf()
	if len(before) != len(after) {
		t.Fatalf("mounted elapsed tooltip texts changed count while closed: %q -> %q", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("closed tooltip elapsed text ticked: %q -> %q", before[i], after[i])
		}
	}
	// 选择器本身必须有效：真正打开 tooltip 后仍要找到详细工作时间。
	// 否则上面的比较会在实现完全没有挂载该文本时空转。
	gui.Hover("id:terminal-tab-1!9")
	gui.Advance(600 * time.Millisecond)
	gui.Find("text:已工作时间")
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

// 点击终端 tab 的“标题文字”必须切换终端。这个测试用标题文字在 tab 内
// 的真实坐标做命中测试，覆盖“已经在一个终端页时，再点另一个终端 tab，
// 内容不能继续停留在第一个终端”的回归。
func TestTerminalTabTitlePressSwitchesTerminal(t *testing.T) {
	gui, m, sess := headless(t, app.Options{})
	attach := &fakeAttach{stream: newFakeStream()}
	sess.attach = attach.attach
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	snap.Status = append(snap.Status, scheduler.TaskRuntime{
		Key:       graph.Key{ProjectID: 1, IID: 10},
		Addr:      "t2",
		Label:     "proj-issue-10",
		StartedAt: started.Add(time.Minute),
	})
	loadGraph(gui, m, snap)
	if len(m.terms.tabs) != 2 {
		t.Fatalf("terminal model tabs = %d, want 2", len(m.terms.tabs))
	}
	gui.Post(func() { m.tab.Set("1!9") })
	gui.Settle()

	title := "#10 · " + started.Add(time.Minute).Format("15:04:05")
	text := gui.Find("text:" + title)
	at := core.Point{X: text.Frame.Size.Width / 2, Y: text.Frame.Size.Height / 2}
	gui.PressAt("text:"+title, at)
	if got := m.tab.GetUntracked(); got != "1!10" {
		t.Fatalf("pressing the second terminal tab title selected %q, want 1!10", got)
	}
	gui.Find("text:地址 t2")

	// 再切回第一个终端，确认这不是只在第一次跳转时碰巧重建。
	firstTitle := "#9 · " + started.Format("15:04:05")
	firstText := gui.Find("text:" + firstTitle)
	gui.PressAt("text:"+firstTitle, core.Point{X: firstText.Frame.Size.Width / 2, Y: firstText.Frame.Size.Height / 2})
	if got := m.tab.GetUntracked(); got != "1!9" {
		t.Fatalf("pressing the first terminal tab title selected %q, want 1!9", got)
	}
	gui.Find("text:地址 t1")
}

// 当前页是依赖图时，点击终端 tab 的标题也必须切到终端：图画布的
// 平移/缩放手势层只能覆盖图本身，不能向上盖住 tab 栏。
func TestTerminalTabTitlePressWorksAboveGraph(t *testing.T) {
	gui, m, sess := headless(t, app.Options{})
	attach := &fakeAttach{stream: newFakeStream()}
	sess.attach = attach.attach
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	loadGraph(gui, m, snap)
	if got := m.tab.GetUntracked(); got != pageGraph {
		t.Fatalf("initial tab = %q, want graph", got)
	}

	title := "#9 · " + started.Format("15:04:05")
	text := gui.Find("text:" + title)
	gui.PressAt("text:"+title, core.Point{X: text.Frame.Size.Width / 2, Y: text.Frame.Size.Height / 2})
	if got := m.tab.GetUntracked(); got != "1!9" {
		t.Fatalf("pressing the terminal tab title above the graph selected %q, want 1!9", got)
	}
	gui.Find("text:地址 t1")
}

// 依赖图内部可以继续显示抓取光标；一旦指针来到 tab 或顶部设置，
// 光标必须回到根视图显式声明的默认箭头。这个显式默认值同时避免
// Linux/GTK 把上一次画布光标作为继承状态留在整个窗口上。
func TestCursorRestoresDefaultOutsideGraph(t *testing.T) {
	if nsgotest.Target() != "" {
		t.Skip("ActiveCursor 只反映无窗口命中模型")
	}
	gui, m, sess := headless(t, app.Options{})
	attach := &fakeAttach{stream: newFakeStream()}
	sess.attach = attach.attach
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	loadGraph(gui, m, snap)

	gui.Hover("id:graph-pointer")
	if got := gui.ActiveCursor().Cursor; got != core.CursorGrab {
		t.Fatalf("cursor over graph = %v, want grab", got)
	}
	title := "#9 · " + started.Format("15:04:05")
	gui.Hover("text:" + title)
	if got := gui.ActiveCursor().Cursor; got != core.CursorDefault {
		t.Fatalf("cursor over terminal tab after graph = %v, want default", got)
	}
	gui.Hover("id:open-settings")
	if got := gui.ActiveCursor().Cursor; got != core.CursorDefault {
		t.Fatalf("cursor over settings after graph = %v, want default", got)
	}
}

// 终端屏幕用文本光标，但离开屏幕后同样要恢复默认箭头；否则从图节点
// 跳到终端后，整个窗口会像仍处在终端文本区一样显示文本光标。
func TestCursorRestoresDefaultOutsideTerminal(t *testing.T) {
	if nsgotest.Target() != "" {
		t.Skip("ActiveCursor 只反映无窗口命中模型")
	}
	gui, m, sess := headless(t, app.Options{})
	attach := &fakeAttach{stream: newFakeStream()}
	sess.attach = attach.attach
	snap := specSnapshot()
	started := gui.Clock.WallTime()
	snap.Status[0].StartedAt = started
	loadGraph(gui, m, snap)

	title := "#9 · " + started.Format("15:04:05")
	gui.Press("text:" + title)
	gui.Find("id:terminal")
	gui.Hover("id:terminal-screen")
	if got := gui.ActiveCursor().Cursor; got != core.CursorText {
		t.Fatalf("cursor over terminal screen = %v, want text", got)
	}
	gui.Hover("text:" + title)
	if got := gui.ActiveCursor().Cursor; got != core.CursorDefault {
		t.Fatalf("cursor over terminal tab after terminal = %v, want default", got)
	}
	gui.Hover("id:open-settings")
	if got := gui.ActiveCursor().Cursor; got != core.CursorDefault {
		t.Fatalf("cursor over settings after terminal = %v, want default", got)
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
	gui.Find("id:settings-reviewer")
	gui.Find("id:settings-hc-binary")
	gui.Find("id:settings-hc-socket")
	gui.Find("id:settings-instance-id")
	gui.Press("id:settings-tab-prompt")
	gui.Find("id:settings-prompt")
}

// 空 issue 启动时，如果发现 dead supervisor 留下的可接管 metadata，GUI 应
// 自动恢复原 spec，而不是要求用户重新输入编号。
func TestEmptyStartAutoRecoversDeadActiveRun(t *testing.T) {
	repoDir := t.TempDir()
	writeGUIActiveMeta(t, guiActiveMeta(repoDir, 8, 9, -1, time.Now()))
	started := make(chan int, 1)
	gui, m, _ := headlessRecovery(t, func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	}, func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		started <- opts.SpecIID
		return nil
	})
	sess := m.session
	waitPaths(t, gui, m)
	select {
	case spec := <-started:
		if spec != 8 {
			t.Fatalf("auto recovered spec = %d, want 8", spec)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the GUI did not auto-start recovery; snapshot = %+v", sess.Snapshot())
	}
	if m.issueText.GetUntracked() != "8" {
		t.Fatalf("issue input = %q, want the recovered spec filled", m.issueText.GetUntracked())
	}
}

// 活 supervisor 且 heartbeat 新鲜时，GUI 只能进入只读观察：不能启动本地
// scheduler，更不能重发 prompt/偷锁；终端页只用于查看 hc 现场。
func TestEmptyStartObservesLiveActiveRun(t *testing.T) {
	repoDir := t.TempDir()
	writeGUIActiveMeta(t, guiActiveMeta(repoDir, 8, 9, os.Getpid(), time.Now()))
	ran := make(chan struct{}, 1)
	gui, m, sess := headlessRecovery(t, func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	}, func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		ran <- struct{}{}
		return nil
	})
	waitPaths(t, gui, m)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && sess.Snapshot().Phase != phaseObserving {
		gui.Settle()
		time.Sleep(5 * time.Millisecond)
	}
	snap := sess.Snapshot()
	if snap.Phase != phaseObserving || !snap.Observing {
		t.Fatalf("snapshot = %+v, want read-only observation", snap)
	}
	select {
	case <-ran:
		t.Fatal("a live active supervisor was taken over by the observer GUI")
	default:
	}
	if m.issueText.GetUntracked() != "8" {
		t.Fatalf("issue input = %q, want the observed spec filled", m.issueText.GetUntracked())
	}
	gui.Settle()
	gui.Find("id:terminal-tab-1!9")
	gui.Find("id:observer-page")
	gui.Find("id:observer-run-9")
	gui.Find("text:worktree:")
	gui.Find("text:supervisor pid:")
	gui.Find("text:heartbeat:")
}

// 显式启动 spec #9 时，仓库里无关 spec #8 的 active worker 不能劫持本次
// 启动。空 issue 才扫描全部 active run；显式 issue 只受同 spec 的 active
// metadata 影响。
func TestExplicitSpecIsNotHijackedByOtherActiveSpec(t *testing.T) {
	repoDir := t.TempDir()
	writeGUIActiveMeta(t, guiActiveMeta(repoDir, 8, 9, os.Getpid(), time.Now()))
	started := make(chan int, 1)
	gui, m, sess := headlessRecoveryOpts(t, app.Options{SpecIID: 9}, func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	}, func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		started <- opts.SpecIID
		return nil
	})
	waitPaths(t, gui, m)
	select {
	case spec := <-started:
		if spec != 9 {
			t.Fatalf("explicit startup spec = %d, want 9 rather than unrelated active spec 8", spec)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("explicit spec did not start; snapshot = %+v", sess.Snapshot())
	}
	if sess.Snapshot().Observing {
		t.Fatal("an unrelated active spec put the explicit run into observer mode")
	}
}

// 观察者不是永远只读：它只读轮询本机 metadata；原 supervisor 进程退出后，
// 同一个 GUI 实例应退出观察模式并自动接管原 spec，而不是要求用户重开/重输。
func TestObserverTakesOverAfterSupervisorExits(t *testing.T) {
	repoDir := t.TempDir()
	supervisor := exec.Command("sleep", "30")
	if err := supervisor.Start(); err != nil {
		t.Skipf("cannot start a temporary supervisor process: %v", err)
	}
	defer func() {
		_ = supervisor.Process.Kill()
		_ = supervisor.Wait()
	}()
	writeGUIActiveMeta(t, guiActiveMeta(repoDir, 8, 9, supervisor.Process.Pid, time.Now()))
	oldInterval := observePollIntervalNanos.Swap(int64(5 * time.Millisecond))
	defer observePollIntervalNanos.Store(oldInterval)
	started := make(chan int, 1)
	gui, m, sess := headlessRecovery(t, func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	}, func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		started <- opts.SpecIID
		return nil
	})
	waitPaths(t, gui, m)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && !sess.Snapshot().Observing {
		gui.Settle()
		time.Sleep(5 * time.Millisecond)
	}
	if !sess.Snapshot().Observing {
		t.Fatalf("snapshot = %+v, want initial read-only observation", sess.Snapshot())
	}
	if err := supervisor.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = supervisor.Wait()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case spec := <-started:
			if spec != 8 {
				t.Fatalf("auto takeover spec = %d, want 8", spec)
			}
			return
		default:
		}
		gui.Settle()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("observer did not take over after supervisor exit; snapshot = %+v", sess.Snapshot())
}

// 损坏/新版 active metadata 必须在界面上变成可见的保守失败，而不是
// 让空 issue 首页表现得像本仓库没有任何 active worker。
func TestInvalidActiveMetadataBlocksAutomaticRecoveryVisibly(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(scheduler.ActiveMetaPath(repoDir, 9), []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	gui, _, sess := headlessRecovery(t, func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	}, func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		return nil
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gui.Settle()
		if snap := sess.Snapshot(); snap.Phase == phaseFailed && snap.Err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("invalid active metadata did not surface as a visible failure; snapshot = %+v", sess.Snapshot())
}

// 显式输入 spec 撞上 live supervisor 时也会进入观察模式。此时首次自动启动
// 已经消费过 AutoStarted；原 supervisor 退出后的接管恢复不能因此被挡住，
// 否则“自动接管”只对空 issue 启动有效，对显式恢复场景失效。
func TestExplicitConflictObserverTakesOverAfterSupervisorExits(t *testing.T) {
	repoDir := t.TempDir()
	supervisor := exec.Command("sleep", "30")
	if err := supervisor.Start(); err != nil {
		t.Skipf("cannot start a temporary supervisor process: %v", err)
	}
	defer func() {
		_ = supervisor.Process.Kill()
		_ = supervisor.Wait()
	}()
	meta := guiActiveMeta(repoDir, 8, 9, supervisor.Process.Pid, time.Now())
	oldInterval := observePollIntervalNanos.Swap(int64(5 * time.Millisecond))
	defer observePollIntervalNanos.Store(oldInterval)
	started := make(chan int, 2)
	first := make(chan struct{}, 1)
	first <- struct{}{}
	gui, m, sess := headlessRecoveryOpts(t, app.Options{SpecIID: 8}, func(context.Context, app.Options) (app.PathsPreview, error) {
		return app.PathsPreview{Paths: app.Paths{WorkDir: repoDir + "/work", RepoDir: repoDir, TreeDir: repoDir + "/.worktrees"}}, nil
	}, func(ctx context.Context, opts app.Options, w io.Writer, obs app.Observer) error {
		started <- opts.SpecIID
		select {
		case <-first:
			// 启动扫描时该 metadata 尚不存在，因此显式启动已经消费了
			// AutoStarted；metadata 在 preflight 前出现，冲突只能在 runFn
			// 中才发现。正是这个路径曾经会让观察者退出后无法自动接管。
			writeGUIActiveMeta(t, meta)
			return &app.ActiveRunConflictError{SpecIID: 8, Tasks: []scheduler.ActiveTaskMeta{meta}}
		default:
			return nil
		}
	})
	waitPaths(t, gui, m)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !sess.Snapshot().Observing {
		gui.Settle()
		time.Sleep(5 * time.Millisecond)
	}
	if !sess.Snapshot().Observing {
		t.Fatalf("snapshot = %+v, want explicit active conflict to enter observation", sess.Snapshot())
	}
	// 第一次冲突启动已经发生； recovery 需要的是第二次启动。
	select {
	case spec := <-started:
		if spec != 8 {
			t.Fatalf("initial explicit spec = %d, want 8", spec)
		}
	case <-time.After(time.Second):
		t.Fatal("initial explicit run did not happen")
	}
	if err := supervisor.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = supervisor.Wait()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case spec := <-started:
			if spec != 8 {
				t.Fatalf("takeover spec = %d, want 8", spec)
			}
			return
		default:
		}
		gui.Settle()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("explicit-conflict observer did not take over; snapshot = %+v", sess.Snapshot())
}

// 日志页的复制按钮把全部日志行写进系统剪贴板，按钮短暂显示"已复制"，
// 时钟走过后恢复；日志为空时按钮禁用，不会把空内容刷上剪贴板。
func TestLogCopyButtonCopiesAllLines(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	snap.Log = []string{"line-one", "line-two"}
	loadGraph(gui, m, snap)
	gui.Press("id:tab-log")
	gui.Press("id:copy-log")
	if gui.Clipboard != "line-one\nline-two" {
		t.Fatalf("clipboard = %q, want the joined log lines", gui.Clipboard)
	}
	gui.Find("text:已复制")
	gui.Advance(2 * time.Second)
	if gui.Exists("text:已复制") {
		t.Fatal("copied feedback did not revert after the clock advanced")
	}
}

// 日志页的保存按钮把全部日志行经系统保存对话框写进 .log 文件，成功后
// 按钮短暂显示"已保存"。
func TestLogSaveButtonSavesLogFile(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	snap := specSnapshot()
	snap.Log = []string{"line-one", "line-two"}
	loadGraph(gui, m, snap)
	path := filepath.Join(t.TempDir(), "saved.log")
	gui.SaveFile = func(r nsgotest.FileSave) string {
		if !strings.HasPrefix(r.Name, "fastimpl-log-") || !strings.HasSuffix(r.Name, ".log") {
			t.Fatalf("save name = %q, want a fastimpl-log .log name", r.Name)
		}
		if string(r.Data) != "line-one\nline-two" {
			t.Fatalf("save data = %q, want the joined log lines", r.Data)
		}
		return path
	}
	gui.Press("id:tab-log")
	gui.Press("id:save-log")
	if len(gui.Saves) != 1 {
		t.Fatalf("save requests = %d, want 1", len(gui.Saves))
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "line-one\nline-two" {
		t.Fatalf("saved log = %q, %v", b, err)
	}
	gui.Find("text:已保存")
	gui.Advance(2 * time.Second)
	if gui.Exists("text:已保存") {
		t.Fatal("saved feedback did not revert after the clock advanced")
	}
}

// 终端页的复制按钮把屏幕与回滚文本写进系统剪贴板：agent TUI 捕获鼠标
// 时拖拽选择不可用，这个按钮是不依赖快捷键的复制通道。终端输出到达
// 是异步的，按压放进 WaitUntil 里等解析完成。
func TestTerminalCopyButtonCopiesScreenText(t *testing.T) {
	gui, m, sess := headless(t, app.Options{})
	stream := newFakeStream()
	sess.attach = (&fakeAttach{stream: stream}).attach
	loadGraph(gui, m, specSnapshot())
	gui.Post(func() { m.tab.Set("1!9") })
	gui.Settle()
	gui.Find("id:copy-terminal")
	stream.readCh <- []byte("copy-me-marker")
	gui.WaitUntil(3*time.Second, func() bool {
		gui.Press("id:copy-terminal")
		return strings.Contains(gui.Clipboard, "copy-me-marker")
	})
}

// 终端页的保存按钮把屏幕与回滚文本按纯文本写进 .md 文件：按用户要求
// 只改扩展名，不额外包 Markdown 代码块。终端输出到达是异步的，按压放进
// WaitUntil 里等解析完成。
func TestTerminalSaveButtonSavesMarkdownFile(t *testing.T) {
	gui, m, sess := headless(t, app.Options{})
	stream := newFakeStream()
	sess.attach = (&fakeAttach{stream: stream}).attach
	loadGraph(gui, m, specSnapshot())
	path := filepath.Join(t.TempDir(), "terminal.md")
	gui.SaveFile = func(r nsgotest.FileSave) string {
		if !strings.HasPrefix(r.Name, "fastimpl-terminal-") || !strings.HasSuffix(r.Name, ".md") {
			t.Fatalf("save name = %q, want a fastimpl-terminal .md name", r.Name)
		}
		return path
	}
	gui.Post(func() { m.tab.Set("1!9") })
	gui.Settle()
	gui.Find("id:save-terminal")
	stream.readCh <- []byte("save-me-marker")
	gui.WaitUntil(3*time.Second, func() bool {
		gui.Press("id:save-terminal")
		return len(gui.Saves) > 0 && strings.Contains(string(gui.Saves[len(gui.Saves)-1].Data), "save-me-marker")
	})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "save-me-marker") {
		t.Fatalf("saved terminal = %q, want the marker", b)
	}
	if strings.Contains(string(b), "```") {
		t.Fatalf("saved terminal = %q, want raw text without a Markdown fence", b)
	}
	gui.Find("text:已保存")
}
