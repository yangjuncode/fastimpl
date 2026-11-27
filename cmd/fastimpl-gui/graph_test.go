package main

import (
	"strings"
	"testing"

	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// specSnapshot 是一份用于绘图测试的快照：同项目 #10 依赖 #9，跨仓库
// 2!3 已关闭且被 #9 依赖，父 issue 自己依赖 2!3。
func specSnapshot() snapshot {
	spec := graph.Key{ProjectID: 1, IID: 8}
	return snapshot{
		Phase: phaseScheduling,
		Spec:  specView{Key: spec, Title: "spec title", Open: true, Number: 8, Deps: []graph.Key{{ProjectID: 2, IID: 3}}},
		Nodes: []graph.Node{
			{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild,
				Deps: []graph.Key{{ProjectID: 2, IID: 3}}},
			{Key: graph.Key{ProjectID: 1, IID: 10}, Title: "task B", Kind: graph.KindChild,
				Deps: []graph.Key{{ProjectID: 1, IID: 9}}},
			{Key: graph.Key{ProjectID: 2, IID: 3}, Title: "cross repo blocker", Kind: graph.KindExternal, Closed: true},
		},
		Ready: []graph.Key{{ProjectID: 1, IID: 9}},
		Status: []scheduler.TaskRuntime{
			{Key: graph.Key{ProjectID: 1, IID: 9}, Addr: "t1", Label: "proj-issue-9"},
		},
	}
}

func TestDiagramEdgeDirection(t *testing.T) {
	d := buildDiagram(specSnapshot())
	// 每条边都是 前置 → 被阻塞 的一方。
	want := map[[2]string]bool{
		{"2!3", "1!9"}:  true,
		{"1!9", "1!10"}: true,
		{"2!3", "1!8"}:  true, // 父 issue 自己的 blocked-by 画成虚线
	}
	if len(d.edges) != len(want) {
		t.Fatalf("edges = %+v, want %d", d.edges, len(want))
	}
	for _, e := range d.edges {
		key := [2]string{e.Source, e.Target}
		if !want[key] {
			t.Errorf("unexpected edge %s → %s", e.Source, e.Target)
		}
		if e.Target == "1!8" && !e.Dashed {
			t.Errorf("spec edge %s → %s is not dashed", e.Source, e.Target)
		}
		if e.Source == "1!10" {
			t.Errorf("edge points from the blocked issue to its dependency: %s → %s", e.Source, e.Target)
		}
	}
}

func TestDiagramCrossRepoKeysAreDistinct(t *testing.T) {
	d := buildDiagram(specSnapshot())
	ids := map[string]bool{}
	for _, n := range d.nodes {
		ids[flowNodeID(n.Key)] = true
	}
	ids[flowNodeID(d.spec)] = true
	// 同号不同仓库不能混为一谈。
	for _, id := range []string{"1!8", "1!9", "1!10", "2!3"} {
		if !ids[id] {
			t.Errorf("diagram ids %v miss %q", ids, id)
		}
	}
	if ids["1!4"] {
		t.Error("diagram invented a node id")
	}
}

// 边上不出现未知节点：图中的依赖指向闭包外的 key 时忽略这条边，
// 而不是画一条指向空气的线。
func TestDiagramSkipsUnknownEdges(t *testing.T) {
	snap := specSnapshot()
	snap.Nodes[0].Deps = append(snap.Nodes[0].Deps, graph.Key{ProjectID: 9, IID: 99})
	d := buildDiagram(snap)
	for _, e := range d.edges {
		if e.Source == "9!99" || e.Target == "9!99" {
			t.Errorf("edge %s → %s points at a node outside the graph", e.Source, e.Target)
		}
	}
}

func TestDiagramEmpty(t *testing.T) {
	d := buildDiagram(snapshot{})
	if d.spec != (graph.Key{}) || len(d.nodes) != 0 || len(d.edges) != 0 {
		t.Fatalf("diagram = %+v, want empty before a graph is built", d)
	}
	if data := d.flowData(nil); len(data.Nodes) != 0 || len(data.Edges) != 0 {
		t.Fatalf("flow data = %+v, want empty", data)
	}
}

func TestNodeState(t *testing.T) {
	for _, tc := range []struct {
		node graph.Node
		want viewState
	}{
		{graph.Node{Closed: true, State: graph.Running}, viewClosed},
		{graph.Node{Closed: true, State: graph.Integrated}, viewClosed},
		{graph.Node{State: graph.Integrated}, viewIntegrated},
		{graph.Node{State: graph.Finalizing}, viewFinalizing},
		{graph.Node{State: graph.Running}, viewRunning},
		{graph.Node{State: graph.NeedsAttention}, viewNeedsAttention},
		{graph.Node{State: graph.Failed}, viewFailed},
		{graph.Node{State: graph.Pending, InFlight: true}, viewRemote},
		{graph.Node{State: graph.Pending}, viewPending},
	} {
		if got := nodeState(tc.node); got != tc.want {
			t.Errorf("nodeState(%+v) = %v, want %v", tc.node, got, tc.want)
		}
	}
}

func TestSameStructureIgnoresRuntime(t *testing.T) {
	a := buildDiagram(specSnapshot())
	b := buildDiagram(specSnapshot())
	if !sameStructure(a, b) {
		t.Fatal("sameStructure reported a change between identical diagrams")
	}
	// 只有运行状态变化：结构不变，视图因此不重建、相机不动。
	snap := specSnapshot()
	snap.Status[0].Addr = "t9"
	snap.Status[0].Accepted = "abc123"
	snap.Nodes[0].State = graph.Running
	if c := buildDiagram(snap); !sameStructure(a, c) {
		t.Error("sameStructure reported a structural change for a runtime-only update")
	}
	// 结构变化（新增节点）必须被识别。
	snap.Nodes = append(snap.Nodes, graph.Node{Key: graph.Key{ProjectID: 1, IID: 11}, Title: "new"})
	if c := buildDiagram(snap); sameStructure(a, c) {
		t.Error("sameStructure missed an added node")
	}
	// 依赖变化也必须被识别。
	snap = specSnapshot()
	snap.Nodes[1].Deps = nil
	if c := buildDiagram(snap); sameStructure(a, c) {
		t.Error("sameStructure missed a removed dependency")
	}
}

func TestDisplayKey(t *testing.T) {
	spec := graph.Key{ProjectID: 1, IID: 8}
	if got := displayKey(spec, graph.Key{ProjectID: 1, IID: 9}); got != "#9" {
		t.Errorf("displayKey(same project) = %q, want %q", got, "#9")
	}
	if got := displayKey(spec, graph.Key{ProjectID: 2, IID: 9}); got != "2!9" {
		t.Errorf("displayKey(cross project) = %q, want %q", got, "2!9")
	}
}

func TestProgressMessageNamesPhases(t *testing.T) {
	for _, tc := range []struct {
		p    graph.BuildProgress
		want string
	}{
		{graph.BuildProgress{Phase: graph.PhaseSpec}, "父 issue"},
		{graph.BuildProgress{Phase: graph.PhaseSeeds}, "子任务"},
		{graph.BuildProgress{Phase: graph.PhaseExpand, Discovered: 5, Read: 3}, "已发现 5 个节点，已读取 3 个"},
		{graph.BuildProgress{Phase: graph.PhaseCheck, Discovered: 5}, "依赖已收敛"},
		{graph.BuildProgress{Phase: graph.PhaseDone, Discovered: 5, Open: 4, Closed: 1}, "4 开放 / 1 已关闭"},
	} {
		if got := progressMessage(tc.p); !strings.Contains(got, tc.want) {
			t.Errorf("progressMessage(%+v) = %q, want it to mention %q", tc.p, got, tc.want)
		}
	}
}

func TestViewReadyName(t *testing.T) {
	if got := viewReady.String(); got != "ready" {
		t.Fatalf("viewReady.String() = %q, want ready", got)
	}
}
