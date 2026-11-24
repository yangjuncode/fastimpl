package main

import (
	"fmt"
	"strings"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	uiflow "github.com/nsgo-dev/nsgo/ui/flow"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// cardWidth 与 cardHeight 固定卡片尺寸：布局因此稳定，状态刷新
// 不会让节点在相机里跳动。
const (
	cardWidth  core.Points = 236
	cardHeight core.Points = 104
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
	}
	return colors.Muted, lucide.CircleDashed, "等待前置"
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
		ID: flowNodeID(d.spec), Label: "spec " + d.title, Width: cardWidth, Height: cardHeight,
		Content: nodeCard{look: look, key: d.spec, spec: d.spec, title: d.title, specCard: true},
	})
	for _, n := range d.nodes {
		data.Nodes = append(data.Nodes, uiflow.Node{
			ID: flowNodeID(n.Key), Label: n.Title, Width: cardWidth, Height: cardHeight,
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
	// Lookup 按 key 返回最新的节点与运行数据。
	Lookup func(graph.Key) (graph.Node, scheduler.TaskRuntime, bool)
}

// read 订阅刷新信号并返回当前值。
func (g *liveGraph) read(cx *core.Scope) int {
	if g == nil || g.Revision == nil {
		return 0
	}
	return g.Revision.Get()
}

// nodeCard 是依赖图里一个节点的卡片。
type nodeCard struct {
	look     *liveGraph
	key      graph.Key
	spec     graph.Key
	title    string
	specCard bool
	// node 是建图时的节点，作为 Lookup 还没算出来时的兜底。
	node graph.Node
}

// Render 实现 core.Node。
func (c nodeCard) Render(cx *core.Scope) core.Node {
	colors := ui.ColorsOf(cx)
	c.look.read(cx)
	node, rt := c.node, scheduler.TaskRuntime{}
	if c.look != nil && c.look.Lookup != nil {
		if live, r, ok := c.look.Lookup(c.key); ok {
			node = live
			rt = r
		}
	}
	if c.specCard {
		node.Title = c.title
	}
	state := nodeState(node)
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
	body := core.Group{
		core.Text{Content: node.Title, MaxLines: 2, Style: ui.TypeBody.Merge(core.Style{FontWeight: ui.Semibold}),
			StyleFunc: ui.Colored(colors.Text)},
		core.Text{Content: displayKey(c.spec, node.Key) + " · " + kind, MaxLines: 1,
			Style: core.Style{FontSize: 11, LineHeight: 15}, StyleFunc: ui.Colored(colors.Muted)},
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
	if state == viewRunning || state == viewFinalizing {
		card.Variant = uiflow.CardAccent
	}
	return card
}
