// Package graph 维护 spec issue 的依赖图与任务状态。
// 节点 = spec 的直接子任务 + 沿 blocked-by 关联传递闭包拉入的全部
// issue（含已关闭的，供画出完整依赖图）。A blocked-by B 记为
// A.Deps 含 B：B 先完成（Closed 或 Integrated）A 才就绪。
package graph

import (
	"fmt"
	"slices"
	"sync"
)

// State 是调度器眼中的任务状态。
type State uint8

const (
	// Pending 表示任务已进入图，但仍有未完成的直接前置任务。
	Pending State = iota
	// Running 表示 worker 会话正在该任务的 worktree 中工作。
	Running
	// NeedsAttention 表示重试耗尽、判定未通过或集成冲突，需要人工处理。
	NeedsAttention
	// Finalizing 表示报告已通过判定，正在串行集成。
	Finalizing
	// Integrated 表示提交已合入集成分支并推送，且 issue 已关闭。
	Integrated
	// Failed 表示该任务在本运行中不可能成功，且不会重试。
	Failed
)

// String 返回状态的稳定名称，供状态视图和日志使用。
func (s State) String() string {
	switch s {
	case Pending:
		return "pending"
	case Running:
		return "running"
	case NeedsAttention:
		return "needs attention"
	case Finalizing:
		return "finalizing"
	case Integrated:
		return "integrated"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Kind 区分节点来源，供画依赖图与策略差异使用。
type Kind uint8

const (
	// KindChild 是 spec issue 的直接子任务。
	KindChild Kind = iota
	// KindExternal 是沿 blocked-by 闭包拉入的 issue（非本 spec 子任务）。
	KindExternal
)

// String 返回节点来源的稳定名称。
func (k Kind) String() string {
	if k == KindExternal {
		return "external"
	}
	return "child"
}

// Key 唯一定位一个 issue：项目 ID 与项目内编号。
type Key struct {
	ProjectID int
	IID       int
}

// String 返回 "project!iid" 形式的稳定标识。
func (k Key) String() string {
	return fmt.Sprintf("%d!%d", k.ProjectID, k.IID)
}

// Node 是依赖图中的一个节点。
type Node struct {
	// Key 是节点在 forge 上的身份（可跨项目）。
	Key Key
	// Title 是 issue 标题。
	Title string
	// Type 是 work item 类型（task/issue 等）。
	Type string
	// Kind 标记节点是 spec 子任务还是外部拉入的 blocker。
	Kind Kind
	// State 是当前调度状态。
	State State
	// Closed 表示 issue 在 forge 上已关闭（视为已完成，不再调度，
	// 但保留在图中供画依赖图）。
	Closed bool
	// InFlight 表示 issue 在 forge 的看板 status 为进行中——他人或
	// 别的 agent 正在处理（周期性刷新的结果）；不就绪，看板退回
	// 空闲后自动恢复调度。
	InFlight bool
	// Deps 是本项 blocked-by 的目标：全部 Closed 或 Integrated 后本项就绪。
	Deps []Key
}

// Graph 拥有依赖图与任务状态；所有读写都在 mu 下完成。
type Graph struct {
	mu sync.Mutex
	// spec 是父 issue 的身份。
	spec Key
	// specTitle 是父 issue 标题。
	specTitle string
	// specDeps 是 spec 自身 blocked-by 的目标（只展示，不 gate 子任务）。
	specDeps []Key
	// nodes 是全部节点（子任务 + 传递闭包的外部 blocker）。
	nodes map[Key]*Node
}

// New 创建只含 spec 身份的空图。
func New(spec Key) *Graph {
	return &Graph{
		spec:  spec,
		nodes: make(map[Key]*Node),
	}
}

// Spec 返回父 issue 的身份。
func (g *Graph) Spec() Key { return g.spec }

// SpecTitle 返回父 issue 标题。
func (g *Graph) SpecTitle() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.specTitle
}

// SpecDeps 返回 spec 自身 blocked-by 的目标（展示用）。
func (g *Graph) SpecDeps() []Key {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Key{}, g.specDeps...)
}

// Add 把一个节点加入图；重复 Key 会覆盖（用于刷新）。
func (g *Graph) Add(n Node) {
	g.mu.Lock()
	defer g.mu.Unlock()
	node := n
	g.nodes[n.Key] = &node
}

// Lookup 返回一个节点的副本；不存在时 ok=false。
func (g *Graph) Lookup(k Key) (Node, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.nodes[k]; ok {
		return *n, true
	}
	return Node{}, false
}

// SetState 更新一个节点状态；节点不存在时忽略。
func (g *Graph) SetState(k Key, s State) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.nodes[k]; ok {
		n.State = s
	}
}

// SetClosed 标记节点在 forge 上的开关状态（周期性刷新的结果）。
func (g *Graph) SetClosed(k Key, closed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.nodes[k]; ok {
		n.Closed = closed
	}
}

// SetInFlight 标记节点在 forge 上的占用状态（周期性刷新的结果）。
func (g *Graph) SetInFlight(k Key, inFlight bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.nodes[k]; ok {
		n.InFlight = inFlight
	}
}

// schedulableLocked 报告节点是否是本运行管理的工作项：只有与 spec
// 同项目的节点才由 agent 执行；跨项目节点（GitLab 跨项目 blocker、
// GitHub 跨仓库 sub-issue）只作为依赖 gate 留在图中。
func (g *Graph) schedulableLocked(n *Node) bool {
	return n.Key.ProjectID == g.spec.ProjectID
}

// readyLocked 报告节点当前是否可调度：issue 未关闭、未被他人占用、
// 状态为 Pending、与 spec 同项目、全部前置节点已关闭或已集成。
// 前置是开放的跨项目节点时本项一直不就绪（外部 blocker 只能等外部关闭）。
func (g *Graph) readyLocked(n *Node) bool {
	if n.Closed || n.State != Pending || n.InFlight || !g.schedulableLocked(n) {
		return false
	}
	for _, dep := range n.Deps {
		depNode, ok := g.nodes[dep]
		if !ok {
			return false
		}
		if !depNode.Closed && depNode.State != Integrated {
			return false
		}
	}
	return true
}

// Ready 返回当前可调度的同项目节点快照（含同项目外部 blocker 工作项）。
func (g *Graph) Ready() []Node {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Node
	for _, n := range g.nodes {
		if g.readyLocked(n) {
			out = append(out, *n)
		}
	}
	slices.SortFunc(out, func(a, b Node) int {
		if a.Key.ProjectID != b.Key.ProjectID {
			return a.Key.ProjectID - b.Key.ProjectID
		}
		return a.Key.IID - b.Key.IID
	})
	return out
}

// OpenKeys 返回全部未关闭节点，供周期性状态刷新使用。
func (g *Graph) OpenKeys() []Key {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Key
	for _, n := range g.nodes {
		if !n.Closed {
			out = append(out, n.Key)
		}
	}
	return out
}

// Done 报告本运行是否结束：每个同项目工作项要么 Closed/Integrated/Failed。
// 跨项目节点不是本运行的工作项，不计入完成条件（开放的跨项目前置
// 会让下游节点一直不就绪，调度器据此判定停滞而不是误判完成）。
func (g *Graph) Done() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range g.nodes {
		if n.Closed || !g.schedulableLocked(n) {
			continue
		}
		switch n.State {
		case Integrated, Failed:
		default:
			return false
		}
	}
	return true
}

// Snapshot 返回按 ProjectID+IID 排序的全部节点副本。
func (g *Graph) Snapshot() []Node {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, *n)
	}
	slices.SortFunc(out, func(a, b Node) int {
		if a.Key.ProjectID != b.Key.ProjectID {
			return a.Key.ProjectID - b.Key.ProjectID
		}
		return a.Key.IID - b.Key.IID
	})
	return out
}
