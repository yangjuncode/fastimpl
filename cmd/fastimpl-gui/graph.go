package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	uiflow "github.com/nsgo-dev/nsgo/ui/flow"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// cardWidth 与 cardHeight 是 flow 为每个节点预留的固定布局尺寸。
// 最高的完结态卡片（开始/结束/用时两行）也放得下，状态与时间刷新
// 因此不会改变拓扑布局，更不会让节点在相机里跳动。
const (
	cardWidth  core.Points = 236
	cardHeight core.Points = 145
)

// viewState 是一个节点在图上显示的状态。
type viewState int

const (
	// viewClosed 是 issue 在 forge 上已关闭。
	viewClosed viewState = iota
	// viewIntegrated 是本次运行已合入集成分支。
	viewIntegrated
	// viewFinalizing 是正在串行集成。
	viewFinalizing
	// viewRunning 是 worker 正在该任务的 worktree 里工作。
	viewRunning
	// viewNeedsAttention 是重试耗尽、冲突或预检受阻，需要人工。
	viewNeedsAttention
	// viewFailed 是本次运行不会再试。
	viewFailed
	// viewRemote 是他人或别的 agent 正在处理（看板进行中）。
	viewRemote
	// viewReady 是前置已全部完成，可以开始工作。
	viewReady
	// viewPending 是已入图，等前置完成。
	viewPending
)

// String 返回状态的稳定名称，供测试与无障碍文本使用。
func (v viewState) String() string {
	switch v {
	case viewClosed:
		return "closed"
	case viewIntegrated:
		return "integrated"
	case viewFinalizing:
		return "finalizing"
	case viewRunning:
		return "running"
	case viewNeedsAttention:
		return "needs attention"
	case viewFailed:
		return "failed"
	case viewRemote:
		return "in progress elsewhere"
	case viewReady:
		return "ready"
	}
	return "pending"
}

// nodeState 把图里的节点合成一个显示状态。已关闭优先于一切：forge
// 上关闭的 issue 不再调度，画成完成最不容易误读；其次是本次运行的
// 状态，最后才是看板占用。
func nodeState(n graph.Node) viewState {
	if n.Closed {
		return viewClosed
	}
	switch n.State {
	case graph.Integrated:
		return viewIntegrated
	case graph.Finalizing:
		return viewFinalizing
	case graph.Running:
		return viewRunning
	case graph.NeedsAttention:
		return viewNeedsAttention
	case graph.Failed:
		return viewFailed
	}
	if n.InFlight {
		return viewRemote
	}
	return viewPending
}

// flowNodeID 是节点在 flow 里的稳定 ID：graph.Key.String()。跨仓库的
// 节点因此不会被误认成同项目的同号 issue。
func flowNodeID(k graph.Key) string { return k.String() }

// displayKey 与 graph 的文本图一致：同项目显示 #iid，跨项目显示 proj!iid。
func displayKey(spec, k graph.Key) string {
	if k.ProjectID == spec.ProjectID {
		return fmt.Sprintf("#%d", k.IID)
	}
	return k.String()
}

// displayTitle 把 issue 编号放进图节点标题，跨项目时保留完整 project!iid。
func displayTitle(spec, k graph.Key, title string) string {
	return displayKey(spec, k) + " " + title
}

// shortSHA 截短提交哈希，供卡片页脚显示。
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// attentionSummary 把 attention 压成一行：卡片只有一行的位置，
// 完整原因在状态栏里。
func attentionSummary(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 56 {
		return string(r[:56]) + "…"
	}
	return s
}

// pendingColor 是“等待前置”的浅橙色：保留警告色的橙色含义，同时
// 降低不透明度，避免小卡片上像失败/告警一样刺眼。
func pendingColor(colors ui.Colors) func() core.Color {
	return func() core.Color { return ui.Alpha(colors.Warn(), 0.8) }
}

// stateBadge 返回状态的颜色、图标与文字。
func stateBadge(colors ui.Colors, v viewState) (func() core.Color, func() *core.SVG, string) {
	switch v {
	case viewClosed:
		return colors.Muted, lucide.CircleCheck, "已关闭"
	case viewIntegrated:
		return colors.Up, lucide.CircleCheck, "已集成"
	case viewFinalizing:
		return colors.Accent, lucide.GitMerge, "集成中"
	case viewRunning:
		return colors.Accent, lucide.LoaderCircle, "运行中"
	case viewNeedsAttention:
		return colors.Warn, lucide.TriangleAlert, "需要处理"
	case viewFailed:
		return colors.Danger, lucide.CircleX, "失败"
	case viewRemote:
		return colors.Info, lucide.CloudCog, "远端进行中"
	case viewReady:
		return colors.Up, lucide.CirclePlay, "可开始工作"
	}
	return pendingColor(colors), lucide.CircleDashed, "等待前置"
}

// diagram 是一次绘图：父 issue、任务节点与有向边。边一律由前置指向
// 被阻塞的一方（dep → node），与 blocked-by 的语义一致；父 issue 自己
// 的 blocked-by 画成虚线。
//
// 它只在图的结构变化时重建（首次建图、刷新改了节点或依赖），节点
// 状态与终端地址的刷新走 nodeCard 自己订阅的信号，因此不会每 tick
// 重建视图或移动相机。
type diagram struct {
	spec  graph.Key
	title string
	open  bool
	deps  []graph.Key
	nodes []graph.Node
	edges []uiflow.Edge
}

// buildDiagram 从一份快照画出结构。runtime 只影响卡片内容，不进结构。
func buildDiagram(snap snapshot) diagram {
	d := diagram{spec: snap.Spec.Key, title: snap.Spec.Title, open: snap.Spec.Open, deps: snap.Spec.Deps}
	if d.spec == (graph.Key{}) {
		return d
	}
	d.nodes = append(d.nodes, snap.Nodes...)
	known := map[string]bool{flowNodeID(d.spec): true}
	for _, n := range d.nodes {
		known[flowNodeID(n.Key)] = true
	}
	for _, n := range d.nodes {
		target := flowNodeID(n.Key)
		for _, dep := range n.Deps {
			id := flowNodeID(dep)
			if !known[id] || id == target {
				continue
			}
			d.edges = append(d.edges, uiflow.Edge{Source: id, Target: target})
		}
	}
	for _, dep := range d.deps {
		id := flowNodeID(dep)
		if !known[id] {
			continue
		}
		d.edges = append(d.edges, uiflow.Edge{Source: id, Target: flowNodeID(d.spec), Dashed: true})
	}
	return d
}

// sameStructure 报告两份快照的图结构是否一致：节点身份、依赖与标题。
// 只有状态、终端地址或提交变了时它返回 true，视图因此不动布局。
func sameStructure(a, b diagram) bool {
	if a.spec != b.spec || a.title != b.title || a.open != b.open || len(a.deps) != len(b.deps) ||
		len(a.nodes) != len(b.nodes) || len(a.edges) != len(b.edges) {
		return false
	}
	for i := range a.deps {
		if a.deps[i] != b.deps[i] {
			return false
		}
	}
	for i := range a.nodes {
		x, y := a.nodes[i], b.nodes[i]
		if x.Key != y.Key || x.Title != y.Title || x.Kind != y.Kind || len(x.Deps) != len(y.Deps) {
			return false
		}
		for j := range x.Deps {
			if x.Deps[j] != y.Deps[j] {
				return false
			}
		}
	}
	for i := range a.edges {
		if a.edges[i].Source != b.edges[i].Source || a.edges[i].Target != b.edges[i].Target ||
			a.edges[i].Dashed != b.edges[i].Dashed {
			return false
		}
	}
	return true
}

// flowData 返回 flow 的节点与边。节点 ID 是 graph.Key.String()，
// 跨刷新保持稳定，flow 因此复用节点内容而不是重建它们。
func (d diagram) flowData(look *liveGraph) uiflow.Data {
	if d.spec == (graph.Key{}) {
		return uiflow.Data{}
	}
	data := uiflow.Data{Edges: d.edges}
	data.Nodes = append(data.Nodes, uiflow.Node{
		ID: flowNodeID(d.spec), Label: displayTitle(d.spec, d.spec, d.title), Width: cardWidth, Height: cardHeight,
		Content: nodeCard{look: look, key: d.spec, spec: d.spec, title: d.title, specCard: true},
	})
	for _, n := range d.nodes {
		data.Nodes = append(data.Nodes, uiflow.Node{
			ID: flowNodeID(n.Key), Label: displayTitle(d.spec, n.Key, n.Title), Width: cardWidth, Height: cardHeight,
			Content: nodeCard{look: look, key: n.Key, spec: d.spec, node: n},
		})
	}
	return data
}

// liveGraph 是卡片读运行状态的地方：它由界面模型持有，提供当前节点
// 状态、运行数据与一个随刷新递增的信号。卡片读信号即订阅，状态变了
// 只有变了的卡片重画。
type liveGraph struct {
	// Revision 递增即表示某处的运行状态变了。
	Revision *core.Signal[int]
	// Now 是界面秒级时钟。只有已经开始且尚未结束的卡片订阅它，
	// 因此完结/未开始的节点不会每秒重画。
	Now *core.Signal[time.Time]
	// Lookup 按 key 返回最新的节点、运行数据、是否 ready 与是否存在。
	Lookup func(graph.Key) (graph.Node, scheduler.TaskRuntime, bool, bool)
}

// formatCardTime 是卡片里的紧凑时间：跨天时仍看得出日期，又不把
// 236pt 宽的节点撑成完整日志时间。
func formatCardTime(t time.Time) string {
	return t.Format("01-02 15:04:05")
}

// nodeTimingLines 组出节点卡片的运行时间。只有 StartedAt 已记录才显示；
// 运行中“用时”算到 now，结束后由 terminalWorkTime 固定到 FinishedAt。
func nodeTimingLines(rt scheduler.TaskRuntime, now time.Time) []string {
	if rt.StartedAt.IsZero() {
		return nil
	}
	lines := []string{"开始 " + formatCardTime(rt.StartedAt) + " · 用时 " + terminalWorkTime(rt, now)}
	if !rt.FinishedAt.IsZero() {
		lines = append(lines, "结束 "+formatCardTime(rt.FinishedAt))
	}
	return lines
}

// nodeLive 是一张卡片关心的全部运行输入。它和日志/其他节点分开比较：
// 日志刷新或别的节点变化只会让这个 memo 重算一次小结构，不会重建卡片。
type nodeLive struct {
	node  graph.Node
	rt    scheduler.TaskRuntime
	ready bool
}

// sameGraphNode 比较一个节点会影响卡片展示的字段，含依赖列表。
func sameGraphNode(a, b graph.Node) bool {
	if a.Key != b.Key || a.Title != b.Title || a.Type != b.Type || a.Kind != b.Kind ||
		a.State != b.State || a.Closed != b.Closed || a.InFlight != b.InFlight || len(a.Deps) != len(b.Deps) {
		return false
	}
	for i := range a.Deps {
		if a.Deps[i] != b.Deps[i] {
			return false
		}
	}
	return true
}

// sameNodeLive 是 nodeLive memo 的 equality。TaskRuntime 可比较；Node
// 因 Deps 切片需要逐项比较。
func sameNodeLive(a, b nodeLive) bool {
	return a.rt == b.rt && a.ready == b.ready && sameGraphNode(a.node, b.node)
}

// nodeCard 是依赖图里一个节点的卡片。
type nodeCard struct {
	look     *liveGraph
	key      graph.Key
	spec     graph.Key
	specCard bool
	title    string
	node     graph.Node
}

// readLive 读取这张卡片当前的节点与运行数据。它必须在 memo/effect 里
// 调用，Revision.Get 才会形成订阅。
func (c nodeCard) readLive() nodeLive {
	out := nodeLive{node: c.node}
	if c.look != nil && c.look.Revision != nil {
		c.look.Revision.Get()
	}
	if c.look != nil && c.look.Lookup != nil {
		if live, rt, ready, ok := c.look.Lookup(c.key); ok {
			out.node, out.rt, out.ready = live, rt, ready
		}
	}
	if c.specCard {
		out.node.Key = c.key
		out.node.Title = c.title
	}
	return out
}

// Render 实现 core.Node。外层 Dynamic 只订阅“这张卡片相关输入”的 memo：
// 状态/地址/结束时间变化时重建卡片；秒级 elapsed 不在这里订阅，否则每个
// 运行节点每秒都会整卡卸载重挂，图稍大就会卡。
func (c nodeCard) Render(cx *core.Scope) core.Node {
	live := core.NewMemoFunc(cx, c.readLive, sameNodeLive)
	return core.Dynamic(func(cx *core.Scope) core.Node {
		return c.render(cx, live.Get(), live)
	})
}

// render 生成当前状态的卡片。只有运行中的 elapsed 文本单独订阅 Now，
// 因此秒级 tick 只更新那一个 text view，不碰卡片结构、布局与相机。
func (c nodeCard) render(cx *core.Scope, current nodeLive, live *core.Memo[nodeLive]) core.Node {
	colors := ui.ColorsOf(cx)
	node, rt, ready := current.node, current.rt, current.ready
	state := nodeState(node)
	if ready && state == viewPending {
		state = viewReady
	}
	if c.specCard && !node.Closed {
		// 父 issue 本身不进调度：它只表示"这是本次运行的根"。
		state = viewPending
	}
	color, icon, label := stateBadge(colors, state)

	header := core.Group{
		core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 4}, Children: []core.Node{
			ui.Icon(icon, 12, color), ui.Text(label, 11, ui.Medium, color)}},
		ui.Spacer{},
	}
	if rt.Addr != "" {
		header = append(header, ui.Icon(lucide.SquareTerminal, 12, colors.Muted))
	}

	kind := node.Kind.String()
	if c.specCard {
		kind = "spec"
	}
	elapsed := core.Text{MaxLines: 1,
		Style: core.Style{FontSize: 11, LineHeight: 15}, StyleFunc: ui.Colored(colors.Muted),
		ContentFunc: func() string {
			current := live.Get().rt
			if current.StartedAt.IsZero() {
				return ""
			}
			now := time.Now()
			if current.FinishedAt.IsZero() && c.look != nil && c.look.Now != nil {
				now = c.look.Now.Get()
			}
			lines := nodeTimingLines(current, now)
			if len(lines) == 0 {
				return ""
			}
			return lines[0]
		}}
	body := core.Group{
		core.Text{Content: displayTitle(c.spec, c.key, node.Title), MaxLines: 2, Style: ui.TypeBody.Merge(core.Style{FontWeight: ui.Semibold}),
			StyleFunc: ui.Colored(colors.Text)},
		core.Text{Content: kind, MaxLines: 1,
			Style: core.Style{FontSize: 11, LineHeight: 15}, StyleFunc: ui.Colored(colors.Muted)},
	}
	if !rt.StartedAt.IsZero() {
		body = append(body, elapsed)
	}
	if lines := nodeTimingLines(rt, rt.FinishedAt); len(lines) > 1 {
		body = append(body, core.Text{Content: lines[1], MaxLines: 1,
			Style: core.Style{FontSize: 11, LineHeight: 15}, StyleFunc: ui.Colored(colors.Muted)})
	}

	var footer core.Node
	switch {
	case state == viewNeedsAttention || state == viewFailed:
		footer = core.Text{Content: attentionSummary(rt.Attention), MaxLines: 1,
			Style: core.Style{FontSize: 11, LineHeight: 15}, StyleFunc: ui.Colored(colors.Warn)}
	case rt.Accepted != "":
		footer = core.Text{Content: "commit " + shortSHA(rt.Accepted), MaxLines: 1,
			Style: core.Style{FontSize: 11, LineHeight: 15}, StyleFunc: ui.Colored(colors.Muted)}
	case len(node.Deps) > 0:
		deps := make([]string, len(node.Deps))
		for i, dep := range node.Deps {
			deps[i] = displayKey(c.spec, dep)
		}
		footer = core.Text{Content: "前置 " + strings.Join(deps, " · "), MaxLines: 1,
			Style: core.Style{FontSize: 11, LineHeight: 15}, StyleFunc: ui.Colored(colors.Muted)}
	}

	card := uiflow.Card{Width: cardWidth, Header: header, Body: body, Footer: footer}
	if state == viewRunning || state == viewFinalizing || state == viewReady {
		card.Variant = uiflow.CardAccent
	}
	return card
}
