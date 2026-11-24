package graph

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Levels 对全部节点做 Kahn 拓扑分层：无前置的节点在 level 0，
// 其余节点 level = 1 + max(dep.level)。已关闭节点同样参与分层，
// 供画完整依赖图。返回的每层节点按 ProjectID+IID 排序。
func (g *Graph) Levels() [][]Node {
	g.mu.Lock()
	defer g.mu.Unlock()
	level := make(map[Key]int, len(g.nodes))
	var assign func(n *Node) int
	assign = func(n *Node) int {
		if l, ok := level[n.Key]; ok {
			return l
		}
		maxDep := -1
		for _, dep := range n.Deps {
			if depNode, ok := g.nodes[dep]; ok {
				if l := assign(depNode); l > maxDep {
					maxDep = l
				}
			}
		}
		level[n.Key] = maxDep + 1
		return maxDep + 1
	}
	for _, n := range g.nodes {
		assign(n)
	}
	top := 0
	for _, l := range level {
		if l > top {
			top = l
		}
	}
	out := make([][]Node, top+1)
	for _, n := range g.nodes {
		out[level[n.Key]] = append(out[level[n.Key]], *n)
	}
	return out
}

// displayKey 是同项目节点显示为 #iid、跨项目节点显示为 proj!iid 的短格式。
func (g *Graph) displayKey(k Key) string {
	if k.ProjectID == g.spec.ProjectID {
		return fmt.Sprintf("#%d", k.IID)
	}
	return k.String()
}

// WritePlan 把依赖图按拓扑层打印成可读的文本图：
// spec 头 + 每层节点（kind/state/title/deps）。
func (g *Graph) WritePlan(w io.Writer) error {
	spec := g.spec
	var b strings.Builder
	fmt.Fprintf(&b, "spec %s %q\n", g.displayKey(spec), g.SpecTitle())
	if deps := g.SpecDeps(); len(deps) > 0 {
		fmt.Fprintf(&b, "  blocked by: %s\n", g.joinDisplay(deps))
	}
	nodes := g.Snapshot()
	var open, closed, children int
	for _, n := range nodes {
		if n.Closed {
			closed++
		} else {
			open++
		}
		if n.Kind == KindChild {
			children++
		}
	}
	fmt.Fprintf(&b, "%d nodes (%d child, %d external): %d open, %d closed\n\n",
		len(nodes), children, len(nodes)-children, open, closed)
	for level, layer := range g.Levels() {
		fmt.Fprintf(&b, "level %d:\n", level)
		for _, n := range layer {
			state := "open"
			if n.Closed {
				state = "closed"
			} else if n.InFlight {
				state = "in-flight"
			}
			deps := "-"
			if len(n.Deps) > 0 {
				deps = g.joinDisplay(n.Deps)
			}
			fmt.Fprintf(&b, "  %-6s %-8s %-9s %-40s deps: %s\n",
				g.displayKey(n.Key), n.Kind, state, n.Title, deps)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// joinDisplay 把一组 Key 拼成显示格式序列。
func (g *Graph) joinDisplay(keys []Key) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = g.displayKey(k)
	}
	return strings.Join(parts, " ")
}

// jsonNode 是 JSON 输出中的一个节点。
type jsonNode struct {
	Key       string   `json:"key"`
	ProjectID int      `json:"project_id"`
	IID       int      `json:"iid"`
	Title     string   `json:"title"`
	Type      string   `json:"type,omitempty"`
	Kind      string   `json:"kind"`  // child|external
	State     string   `json:"state"` // open|closed
	InFlight  bool     `json:"in_flight,omitempty"`
	Deps      []string `json:"deps"`
}

// jsonGraph 是 JSON 输出的整体结构。
type jsonGraph struct {
	Spec struct {
		Key       string   `json:"key"`
		ProjectID int      `json:"project_id"`
		IID       int      `json:"iid"`
		Title     string   `json:"title"`
		Deps      []string `json:"deps"`
	} `json:"spec"`
	Nodes []jsonNode `json:"nodes"`
}

// WriteJSON 把完整依赖图（含每个节点 open/closed 状态）写成 JSON。
func (g *Graph) WriteJSON(w io.Writer) error {
	var doc jsonGraph
	spec := g.spec
	doc.Spec.Key = spec.String()
	doc.Spec.ProjectID = spec.ProjectID
	doc.Spec.IID = spec.IID
	doc.Spec.Title = g.SpecTitle()
	for _, dep := range g.SpecDeps() {
		doc.Spec.Deps = append(doc.Spec.Deps, dep.String())
	}
	for _, n := range g.Snapshot() {
		state := "open"
		if n.Closed {
			state = "closed"
		}
		jn := jsonNode{
			Key:       n.Key.String(),
			ProjectID: n.Key.ProjectID,
			IID:       n.Key.IID,
			Title:     n.Title,
			Type:      n.Type,
			Kind:      n.Kind.String(),
			State:     state,
			InFlight:  n.InFlight,
			Deps:      make([]string, 0, len(n.Deps)),
		}
		for _, dep := range n.Deps {
			jn.Deps = append(jn.Deps, dep.String())
		}
		doc.Nodes = append(doc.Nodes, jn)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
