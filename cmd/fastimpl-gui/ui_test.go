package main

import (
	"context"
	"testing"

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
		m.snap, m.nodes, m.runtime = snap, nodes, runtime
		m.structure = buildDiagram(snap)
		m.flowReady = true
		m.mu.Unlock()
		m.rev.Update(func(n int) int { return n + 1 })
	})
	app.Settle()
}

// 没传 issue 时主界面是启动表单：issue 编号与三个目录都在，居中显示。
// 目录自动解析成功后会预填，并在底部只读信息栏显示。
func TestEmptyStateRenders(t *testing.T) {
	app, m, _ := headless(t, app.Options{})
	waitPaths(t, app, m)
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
	app.Find("id:legend")
	app.Find("id:graph")
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
