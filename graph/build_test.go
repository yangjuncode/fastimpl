package graph

import (
	"context"
	"strings"
	"testing"
)

// fakeSource 是 graph.Source 的测试实现。
type fakeSource struct {
	spec     IssueInfo
	children []ChildRef
	issues   map[Key]IssueInfo
	blockers map[Key][]Key
}

func (f fakeSource) Spec(ctx context.Context, iid int) (IssueInfo, error) { return f.spec, nil }
func (f fakeSource) Children(ctx context.Context, iid int) ([]ChildRef, error) {
	return f.children, nil
}
func (f fakeSource) BlockedBy(ctx context.Context, k Key) ([]Key, error) {
	return f.blockers[k], nil
}
func (f fakeSource) Issue(ctx context.Context, k Key) (IssueInfo, error) {
	return f.issues[k], nil
}

func testSource() fakeSource {
	return fakeSource{
		spec: IssueInfo{Key: Key{1, 8}, Title: "spec title", Type: "issue", Open: true},
		children: []ChildRef{
			{Key: Key{1, 9}, Title: "task A", State: "opened", Type: "task"},
			{Key: Key{1, 10}, Title: "task B", State: "opened", Type: "task"},
			{Key: Key{1, 11}, Title: "not a task", State: "opened", Type: "issue"},
		},
		issues: map[Key]IssueInfo{
			{1, 9}:  {Key: Key{1, 9}, Title: "task A", Type: "task", Open: true},
			{1, 10}: {Key: Key{1, 10}, Title: "task B", Type: "task", Open: true},
			{1, 20}: {Key: Key{1, 20}, Title: "ext base", Type: "task", Open: false},
			{1, 21}: {Key: Key{1, 21}, Title: "ext open", Type: "task", Open: true},
		},
		blockers: map[Key][]Key{
			{1, 9}:  {{ProjectID: 1, IID: 20}},
			{1, 10}: {{ProjectID: 1, IID: 9}, {ProjectID: 1, IID: 21}},
			{1, 21}: {},
		},
	}
}

func TestBuildTransitiveClosure(t *testing.T) {
	g, err := Build(context.Background(), testSource(), 8)
	if err != nil {
		t.Fatal(err)
	}
	nodes := g.Snapshot()
	if len(nodes) != 4 {
		t.Fatalf("got %d nodes, want 4 (tasks 9,10 + ext 20,21; non-task child 11 excluded)", len(nodes))
	}
	ext, ok := g.Lookup(Key{1, 20})
	if !ok || ext.Kind != KindExternal || !ext.Closed {
		t.Errorf("node 1!20 = %+v, want external closed", ext)
	}
	// #9 只依赖已关闭的 #20 → 就绪；#21 无前置 → 就绪；
	// #10 依赖未完成的 #9/#21 → 不就绪。
	ready := g.Ready()
	if len(ready) != 2 {
		t.Fatalf("ready = %v, want #9 and #21", ready)
	}
	ids := map[int]bool{ready[0].Key.IID: true, ready[1].Key.IID: true}
	if !ids[9] || !ids[21] {
		t.Fatalf("ready = %v, want #9 and #21", ready)
	}
}

func TestBuildCycle(t *testing.T) {
	src := testSource()
	src.issues[Key{1, 9}] = IssueInfo{Key: Key{1, 9}, Title: "task A", Type: "task", Open: true}
	src.blockers[Key{1, 9}] = []Key{{1, 10}}
	src.blockers[Key{1, 10}] = []Key{{1, 9}}
	if _, err := Build(context.Background(), src, 8); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestWritePlan(t *testing.T) {
	g, err := Build(context.Background(), testSource(), 8)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := g.WritePlan(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"spec #8", "level 0:", "level 1:", "#9", "task A", "external"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	g, err := Build(context.Background(), testSource(), 8)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := g.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{`"state": "closed"`, `"state": "open"`, `"kind": "external"`, `"1!20"`} {
		if !strings.Contains(out, want) {
			t.Errorf("json output missing %q:\n%s", want, out)
		}
	}
}
