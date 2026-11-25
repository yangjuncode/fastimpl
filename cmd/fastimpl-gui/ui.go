package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/nsgo-dev/nsgo/base/camera"
	"github.com/nsgo-dev/nsgo/base/terminal"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	uiflow "github.com/nsgo-dev/nsgo/ui/flow"
	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
	"github.com/yangjuncode/fastimpl/version"
)

// model 是界面的全部状态。所有字段只在界面线程上读写，除了 snap 与
// 索引：它们由 mu 保护，因为后台的运行 goroutine 会写。
type model struct {
	cx      *core.Scope
	session *session
	// opts 是这次运行从命令行得到的选项；界面上的仓库目录覆盖只写进
	// 这里的新副本，绝不持久化。
	opts app.Options

	mu   sync.Mutex
	snap snapshot
	// nodes、runtime 与 ready 是渲染时用的索引，由 snap 派生。
	nodes   map[graph.Key]graph.Node
	runtime map[graph.Key]scheduler.TaskRuntime
	ready   map[graph.Key]bool
	// structure 是上一次建好的图结构；只有结构变了才重建。
	structure diagram
	flowReady bool

	// rev 随状态刷新递增，全部读状态的界面元素读它即订阅变化。
	rev *core.Signal[int]

	// issueText 是录入的 issue 编号。
	issueText *core.Signal[string]
	// workText、repoText、treeText 是三个目录：工作目录、仓库目录、
	// worktree 目录。自动解析成功的值会预填进来；用户改过的值只用于
	// 本次运行，绝不写回配置。
	workText, repoText, treeText *core.Signal[string]
	// settingsOpen 控制设置对话框；settings 持有配置与提示词编辑状态。
	settingsOpen *core.Signal[bool]
	settings     *settingsModel
	// tab 是当前选中的页面：pageGraph 或某个终端页的 ID。
	tab *core.Signal[string]
	// selected 是图上选中的节点 ID。
	selected *core.Signal[string]

	cam       *camera.State
	flowState *uiflow.State

	terms *terminals

	// post 把一次界面更新投到引擎的队列上；生产里是 cx.Post，
	// 测试里换成记录器。
	post func(func())

	// pending 与 posted 把后台通知合并成一次界面更新。
	pending bool
	posted  bool
}

// pageGraph 是依赖图页的标识，也是 tab 的第一项。
const pageGraph = "graph"

// newModel 组装界面模型：它接管会话的事件，并在界面线程上合并刷新。
func newModel(cx *core.Scope, s *session, opts app.Options) *model {
	m := &model{
		cx: cx, session: s, opts: opts,
		rev:          core.NewSignal(cx, 0),
		issueText:    core.NewSignal(cx, ""),
		workText:     core.NewSignal(cx, ""),
		repoText:     core.NewSignal(cx, ""),
		treeText:     core.NewSignal(cx, ""),
		settingsOpen: core.NewSignal(cx, false),
		tab:          core.NewSignal(cx, pageGraph),
		selected:     core.NewSignal(cx, ""),
		cam:          &camera.State{},
		flowState:    &uiflow.State{},
	}
	m.post = cx.Post
	m.settings = newSettingsModel(cx)
	m.terms = newTerminals(s, m.notify)
	if opts.SpecIID > 0 {
		m.issueText.Set(strconv.Itoa(opts.SpecIID))
	}
	s.SetOnChange(m.notify)
	cx.Cleanup(func() { m.terms.closeAll() })
	// 启动即预览三个目录：工作目录默认就是 GUI 的当前目录，仓库目录按
	// 既有规则定位，worktree 目录按既有优先级推断。只读本地，不联网。
	s.Discover(opts)
	return m
}

// notify 由运行 goroutine 调用：它只排队，不做任何界面工作，因此
// 不会在 hc/网络线程上碰 NSGo 的图。
func (m *model) notify() {
	m.mu.Lock()
	m.pending = true
	already := m.posted
	m.posted = true
	m.mu.Unlock()
	if already {
		return
	}
	m.post(m.flush)
}

// flush 在界面线程上把排队的通知合并成一次状态更新。
func (m *model) flush() {
	m.mu.Lock()
	m.posted = false
	pending := m.pending
	m.pending = false
	m.mu.Unlock()
	if !pending {
		return
	}
	m.sync()
}

// sync 读一次会话快照，同步终端页与图结构，并递增刷新信号。
func (m *model) sync() {
	snap := m.session.Snapshot()
	nodes := make(map[graph.Key]graph.Node, len(snap.Nodes))
	runtime := make(map[graph.Key]scheduler.TaskRuntime, len(snap.Status))
	ready := make(map[graph.Key]bool, len(snap.Ready))
	for _, n := range snap.Nodes {
		nodes[n.Key] = n
	}
	for _, rt := range snap.Status {
		runtime[rt.Key] = rt
	}
	for _, key := range snap.Ready {
		ready[key] = true
	}
	next := buildDiagram(snap)
	rebuild := !sameStructure(m.structure, next)
	m.mu.Lock()
	m.snap, m.nodes, m.runtime, m.ready = snap, nodes, runtime, ready
	if rebuild {
		m.structure = next
	}
	firstGraph := !m.flowReady && snap.GraphReady
	if firstGraph {
		m.flowReady = true
	}
	m.mu.Unlock()

	// 终端页跟着调度器状态走：每次刷新只同步页的存在与地址，
	// 连接在页第一次显示时才建立。
	m.terms.sync(snap.Status)

	m.rev.Update(func(n int) int { return n + 1 })
	if firstGraph {
		// 图刚建好：等布局量出尺寸后把相机对准整张图。
		m.post(m.cam.Fit)
	}
	// 自动解析出的目录预填进输入框，用户已手填的不覆盖。
	if snap.Paths.WorkDir != "" && m.workText.GetUntracked() == "" {
		m.workText.Set(snap.Paths.WorkDir)
	}
	if snap.Paths.RepoDir != "" && m.repoText.GetUntracked() == "" {
		m.repoText.Set(snap.Paths.RepoDir)
	}
	if snap.Paths.TreeDir != "" && m.treeText.GetUntracked() == "" {
		m.treeText.Set(snap.Paths.TreeDir)
	}
	if snap.RepoError != nil && !m.settingsOpen.GetUntracked() {
		if repo := snap.RepoError.Dir; repo != "" && m.repoText.GetUntracked() == "" {
			m.repoText.Set(repo)
		}
		m.openSettings()
	}
	// 带 issue 启动时，路径全部就绪后才自动开始，且只自动开始一次：
	// 路径缺失时停在居中的启动表单，等用户补齐。
	if snap.Running || snap.AutoStarted || snap.Phase != phaseIdle {
		return
	}
	if snap.Issue > 0 && m.issueText.GetUntracked() != "" {
		// 预览还没跑完时不要急着判定缺失。
		if snap.PathsReady {
			m.session.MarkAutoStarted()
			m.start()
		}
	}
}

// snapshot 返回当前快照；它先读刷新信号，因此调用它的渲染代码会
// 随着状态变化重画。
func (m *model) snapshot() snapshot {
	m.rev.Get()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap
}

// look 返回卡片读运行状态的入口。
func (m *model) look() *liveGraph {
	return &liveGraph{Revision: m.rev, Lookup: func(k graph.Key) (graph.Node, scheduler.TaskRuntime, bool, bool) {
		m.rev.Get()
		m.mu.Lock()
		defer m.mu.Unlock()
		n, ok := m.nodes[k]
		return n, m.runtime[k], m.ready[k], ok
	}}
}

// structureOf 返回缓存的图结构；它只在结构变化时被 sync 替换。
func (m *model) structureOf() diagram {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.structure
}

// start 用界面上的 issue 编号与三个目录启动一次运行。
func (m *model) start() {
	opts := m.opts
	text := strings.TrimSpace(m.issueText.GetUntracked())
	iid, err := strconv.Atoi(strings.TrimPrefix(text, "#"))
	if err != nil || iid <= 0 {
		m.opts.SpecIID = 0
		m.session.failLocal(fmt.Errorf("issue 编号必须是正整数，当前是 %q", text))
		m.notify()
		return
	}
	opts.SpecIID = iid
	opts.WorkDir = strings.TrimSpace(m.workText.GetUntracked())
	opts.RepoDir = strings.TrimSpace(m.repoText.GetUntracked())
	opts.TreeDir = strings.TrimSpace(m.treeText.GetUntracked())
	if err := m.session.Start(opts); err != nil {
		m.session.failLocal(err)
		m.notify()
		return
	}
	m.opts = opts
	m.tab.Set(pageGraph)
	m.selected.Set("")
	m.sync()
}

// retry 用设置里的仓库目录重跑同一个 issue。
func (m *model) retry() {
	m.settingsOpen.Set(false)
	m.start()
}

// startScheduling 打开调度门禁：只有图已建好且管线正在等待时才会生效。
func (m *model) startScheduling() {
	m.session.StartScheduling()
	m.sync()
}

// stop 取消运行；调度器的收尾在后台完成，界面状态随之更新。
func (m *model) stop() {
	m.session.Stop()
}

// build 返回整个界面：顶部是运行控制，主体是图/终端页，底部是三个
// 目录的只读信息栏。
func (m *model) build() core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.View{TestID: "root", StyleFunc: func() core.Style {
		return core.Style{Width: core.Pct(100), Height: core.Pct(100), Direction: core.Column, Overflow: core.Hidden,
			Background: colors.Background(), Color: colors.Text()}
	}, Children: []core.Node{
		m.header(),
		core.View{Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Direction: core.Column, Gap: 0, Overflow: core.Hidden},
			Children: []core.Node{m.tabs(), m.body()}},
		m.pathsBar(),
		m.settingsDialog(),
	}}
}

// header 是顶部条：issue 编号、开始/停止与阶段说明。三个目录不在
// 顶部：它们在解析完成后以只读信息栏显示在窗口底部。
func (m *model) header() core.Node {
	colors := ui.ColorsOf(m.cx)
	snap := m.snapshot
	return core.View{TestID: "header", StyleFunc: func() core.Style {
		return core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8, Padding: core.Symmetric(10, 14),
			Borders: core.Edges{Bottom: 1}, BorderColor: colors.Border(), Background: colors.Surface()}
	}, Children: []core.Node{
		ui.Text("fastimpl", 14, ui.Semibold, colors.Text),
		core.Text{Content: version.String(), MaxLines: 1, TestID: "version",
			Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)},
		core.View{Style: core.Style{Width: core.Pt(110)}, Children: []core.Node{
			ui.Input{Value: m.issueText.Get, OnChange: m.issueText.Set, Placeholder: "spec issue", TestID: "issue-input"}}},
		ui.Button{Text: "设置", Variant: ui.ButtonOutline, Icon: lucide.Settings, OnPress: m.openSettings, TestID: "open-settings"},
		ui.Spacer{},
		m.startButton(),
		core.Show{When: func() bool { return snap().Running }, Then: func(*core.Scope) core.Node {
			return ui.Button{Text: "停止", Variant: ui.ButtonDanger, Icon: lucide.Square, OnPress: m.stop, TestID: "stop"}
		}},
		m.statusText(),
	}}
}

// startButton 是主运行按钮。没在运行时它启动依赖图构建；图建好且管线
// 停在门禁前时，它才变成“开始调度”，点击后才会准备集成、连接 hcgo
// 并启动 worker。
func (m *model) startButton() core.Node {
	text := func() string {
		s := m.snapshot()
		switch {
		case s.WaitingStart:
			return "开始调度"
		case s.Running && (s.Phase == phaseStarting || s.Phase == phaseBuilding || s.Phase == phaseReady):
			return "构建中"
		case s.Running:
			return "调度中"
		case s.GraphReady:
			return "重新构建"
		}
		return "构建依赖图"
	}
	busy := func() bool {
		s := m.snapshot()
		return s.Running && !s.WaitingStart
	}
	return ui.Button{TextFunc: text, Variant: ui.ButtonPrimary, Icon: lucide.Play, Busy: busy, Disabled: busy,
		OnPress: func() {
			s := m.snapshot()
			if s.WaitingStart {
				m.startScheduling()
			} else if !s.Running {
				m.start()
			}
		}, TestID: "start"}
}

// pathsBar 是窗口底部的三个目录信息栏：全部解析完成时只读显示。
func (m *model) pathsBar() core.Node {
	colors := ui.ColorsOf(m.cx)
	row := func(label string, read func() string) core.Node {
		return core.View{TestID: "path-" + label, Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 6,
			FlexShrink: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
			ui.Text(label, 11, ui.Medium, colors.Muted),
			core.Text{ContentFunc: read, MaxLines: 1, Style: core.Style{FontSize: 11, LineHeight: 15, FlexShrink: 1},
				StyleFunc: ui.Colored(colors.Text)},
		}}
	}
	return core.View{TestID: "paths-bar", StyleFunc: func() core.Style {
		return core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 18, Padding: core.Symmetric(7, 14),
			Borders: core.Edges{Top: 1}, BorderColor: colors.Border(), Background: colors.Surface()}
	}, Children: []core.Node{
		row("工作目录", m.workText.Get),
		row("仓库目录", m.repoText.Get),
		row("Work Tree 目录", m.treeText.Get),
	}}
}

// statusText 是阶段与构图进度的说明。
func (m *model) statusText() core.Node {
	colors := ui.ColorsOf(m.cx)
	text := func() string {
		s := m.snapshot()
		switch s.Phase {
		case phaseIdle:
			return "输入 issue 编号后构建依赖图；图就绪后再点开始调度"
		case phaseFailed:
			if s.Err != nil {
				return s.Message + "：" + attentionSummary(s.Err.Error())
			}
			return s.Message
		}
		if s.Message == "" {
			return s.Phase.String()
		}
		return s.Message
	}
	color := func() core.Color {
		s := m.snapshot()
		switch s.Phase {
		case phaseFailed:
			return colors.Danger()
		case phaseStopped:
			return colors.Warn()
		case phaseDone:
			return colors.Up()
		}
		return colors.Muted()
	}
	return core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8, MaxWidth: core.Pt(560)},
		Children: []core.Node{
			core.Show{When: func() bool {
				s := m.snapshot()
				return s.Running && (s.Phase == phaseBuilding || s.Phase == phaseStarting || s.Phase == phasePreparing)
			}, Then: func(*core.Scope) core.Node {
				return ui.Spinner{Size: 14, Label: "运行中"}
			}},
			core.Text{ContentFunc: text, MaxLines: 2, Style: ui.TypeCaption, StyleFunc: ui.Colored(color), TestID: "status"},
		}}
}

// pickDir 打开系统的目录选择器，把选中的目录写进对应的输入框。三个
// 目录各有自己的按钮；写入只影响本次运行。
func (m *model) pickDir(title string, start func() string, set func(string)) {
	m.cx.PickDirectories(core.PickDirectoriesOptions{Title: title, Start: start()},
		func(dirs []core.Directory, err error) {
			if err != nil || len(dirs) == 0 {
				return
			}
			if path := dirs[0].Path; path != "" {
				m.cx.Post(func() { set(path) })
			}
		})
}

// pickRepo 选仓库主 checkout（必须是仓库顶层）。
func (m *model) pickRepo() {
	m.pickDir("选择仓库顶层目录", m.repoText.GetUntracked, m.repoText.Set)
}

// pickTree 选 worktree 父目录。
func (m *model) pickTree() {
	m.pickDir("选择 Work Tree 目录", m.treeText.GetUntracked, m.treeText.Set)
}

// pickWork 选 fastimpl 工作目录。
func (m *model) pickWork() {
	m.pickDir("选择工作目录", m.workText.GetUntracked, m.workText.Set)
}

// tabs 是图页与终端页的切换条。
func (m *model) tabs() core.Node {
	colors := ui.ColorsOf(m.cx)
	options := func() []ui.Option[string] {
		m.snapshot()
		opts := []ui.Option[string]{{Value: pageGraph, Label: "依赖图", Icon: lucide.Workflow}}
		for _, tab := range m.terms.tabs {
			label := tab.Label
			if label == "" {
				label = tab.ID()
			}
			opts = append(opts, ui.Option[string]{Value: tab.ID(), Label: label, Icon: lucide.SquareTerminal})
		}
		return opts
	}
	return core.View{TestID: "tabs", StyleFunc: func() core.Style {
		return core.Style{Padding: core.Symmetric(6, 10), Borders: core.Edges{Bottom: 1}, BorderColor: colors.Border()}
	}, Children: []core.Node{
		ui.Tabs[string]{OptionsFunc: options, Value: m.tab.Get, OnChange: m.tab.Set, Size: ui.SizeSmall,
			Label: "页面"},
	}}
}

// body 是当前页。
func (m *model) body() core.Node {
	return core.View{Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
		Children: []core.Node{core.Show{
			When: func() bool { return m.tab.Get() == pageGraph },
			Then: func(cx *core.Scope) core.Node { return m.graphPage() },
			Else: func(cx *core.Scope) core.Node { return m.terminalPage() },
		}}}
}

// graphPage 是依赖图页：可平移缩放的画布、缩放工具条与图例。
func (m *model) graphPage() core.Node {
	return core.View{TestID: "graph-page", Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
		Children: []core.Node{
			core.Show{When: func() bool { return m.snapshot().GraphReady }, Then: func(*core.Scope) core.Node {
				look := m.look()
				return core.View{Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
					Children: []core.Node{
						uiflow.View{Camera: m.cam, State: m.flowState,
							Data: func() uiflow.Data {
								// 只有结构变了才换数据；节点状态的刷新走卡片
								// 自己订阅的信号，不动布局与相机。
								return m.structureOf().flowData(look)
							},
							NodeGap: 28, RankGap: 56,
							Selected: m.selected.Get,
							OnSelect: m.selected.Set,
							OnOpen: func(id string) {
								m.selected.Set(id)
								m.tab.Set(id)
							},
							EmptyText: "依赖图还没有节点",
							LabelFunc: func() string {
								s := m.snapshot()
								return fmt.Sprintf("依赖图：%d 个节点", len(s.Nodes))
							},
							TestID:  "graph",
							Overlay: m.zoomTools(),
						},
						m.legend(),
					}}
			}, Else: func(*core.Scope) core.Node {
				return m.emptyState()
			}},
		}}
}

// zoomTools 浮在画布右上角的缩放控件。
func (m *model) zoomTools() core.Node {
	colors := ui.ColorsOf(m.cx)
	tool := func(icon func() *core.SVG, label string, press func()) core.Node {
		return ui.IconButton{Icon: icon, Label: label, OnPress: press}
	}
	return core.View{TestID: "graph-tools", StyleFunc: func() core.Style {
		return core.Style{Direction: core.Row, Gap: 2, Padding: core.All(4), CornerRadius: ui.RadiusControl + 4,
			BorderWidth: 1, Background: colors.Surface(), BorderColor: colors.Border()}
	}, Children: []core.Node{
		tool(lucide.ZoomOut, "缩小", m.cam.ZoomOut),
		tool(lucide.ZoomIn, "放大", m.cam.ZoomIn),
		tool(lucide.Scan, "适应窗口", m.cam.Fit),
	}}
}

// legend 是画布底部的图例：一眼看懂边的方向与卡片状态。
func (m *model) legend() core.Node {
	colors := ui.ColorsOf(m.cx)
	item := func(icon func() *core.SVG, tint func() core.Color, text string) core.Node {
		return core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 4},
			Children: []core.Node{ui.Icon(icon, 12, tint), ui.Text(text, 11, ui.Regular, colors.Muted)}}
	}
	return core.View{TestID: "legend", StyleFunc: func() core.Style {
		return core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 14, Padding: core.Symmetric(6, 12),
			Borders: core.Edges{Top: 1}, BorderColor: colors.Border(), Background: colors.Surface()}
	}, Children: []core.Node{
		ui.Text("箭头从「前置」指向「被阻塞」的 issue", 11, ui.Regular, colors.Muted),
		item(lucide.CircleDashed, colors.Muted, "等待前置"),
		item(lucide.CirclePlay, colors.Up, "可开始"),
		item(lucide.LoaderCircle, colors.Accent, "运行中"),
		item(lucide.GitMerge, colors.Accent, "集成中"),
		item(lucide.CircleCheck, colors.Up, "已集成"),
		item(lucide.CircleCheck, colors.Muted, "已关闭"),
		item(lucide.TriangleAlert, colors.Warn, "需要处理"),
		item(lucide.CloudCog, colors.Info, "远端进行中"),
	}}
}

// emptyState 是还没有图时的启动表单：issue 编号与三个目录，居中显示。
// 自动解析成功的目录会预填；解析不了的那一项在此填写或选择，补齐后才
// 允许开始。整页铺白时漂浮的几行控件看起来像没画出来，所以放进一张
// 有边界的面。
func (m *model) emptyState() core.Node {
	colors := ui.ColorsOf(m.cx)
	hint := func() string {
		s := m.snapshot()
		switch {
		case s.PathErr != nil:
			return "无法读取工作目录或配置：" + attentionSummary(s.PathErr.Error())
		case s.RepoPathErr != nil:
			return "仓库目录无法自动解析，请填写或选择仓库顶层目录"
		case s.TreePathErr != nil:
			return "Work Tree 目录无法自动解析，请填写或选择"
		case s.PathsReady:
			return "目录已就绪：填写 issue 编号后点构建依赖图；图出来后再点开始调度"
		}
		return "正在解析工作目录、仓库目录与 Work Tree 目录"
	}
	return core.View{TestID: "empty", Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), AlignItems: core.AlignCenter,
		JustifyContent: core.JustifyCenter, Padding: core.All(28)},
		Children: []core.Node{
			core.View{TestID: "empty-panel", Style: core.Style{Width: core.Pt(560), Padding: core.All(24), Gap: 14,
				AlignItems: core.AlignStretch, BorderWidth: 1, CornerRadius: 14},
				StyleFunc: func() core.Style {
					return core.Style{Background: colors.Surface(), BorderColor: colors.Border(),
						Shadows: []core.BoxShadow{ui.Shadow(0, 2, 12, 0, ui.Black(.05))}}
				}, Children: []core.Node{
					core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 10}, Children: []core.Node{
						ui.Icon(lucide.Workflow, 26, colors.Muted),
						core.Text{Content: "输入 spec issue 编号与三个目录，先构建依赖图", MaxLines: 2,
							Style: ui.TypeTitle, StyleFunc: ui.Colored(colors.Text)},
					}},
					m.formRow("issue", "spec issue 编号", m.issueText.Get, m.issueText.Set, nil, nil, "form-issue"),
					m.formRow("工作目录", "fastimpl 运行目录", m.workText.Get, m.workText.Set, m.pickWork, nil, "form-work"),
					m.formRow("仓库目录", "目标仓库顶层目录", m.repoText.Get, m.repoText.Set, m.pickRepo, m.repoPathErrText, "form-repo"),
					m.formRow("Work Tree 目录", "worktree 父目录", m.treeText.Get, m.treeText.Set, m.pickTree, m.treePathErrText, "form-tree"),
					core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 10}, Children: []core.Node{
						ui.Spacer{},
						ui.Button{Text: "构建依赖图", Variant: ui.ButtonPrimary, Icon: lucide.Workflow, OnPress: m.start, TestID: "empty-start"},
					}},
					core.Text{ContentFunc: hint, MaxLines: 3, Style: ui.TypeCaption.Merge(core.Style{TextAlign: core.TextAlignCenter}),
						StyleFunc: ui.Colored(colors.Muted), TestID: "form-hint"},
				}},
		}}
}

// formRow 是启动表单里的一行：标签、输入框，可选的目录选择按钮，以及
// 该行自己的错误说明。pick 为 nil 时没有选择按钮（issue 编号用）。
func (m *model) formRow(label, placeholder string, value func() string, set func(string), pick func(), errText func() string, testID string) core.Node {
	colors := ui.ColorsOf(m.cx)
	children := []core.Node{
		ui.Text(label, 11, ui.Medium, colors.Muted),
		core.View{Style: core.Style{FlexGrow: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
			ui.Input{Value: value, OnChange: set, Placeholder: placeholder, TestID: testID}}},
	}
	if pick != nil {
		children = append(children, ui.Button{Variant: ui.ButtonSecondary, Text: "选择", Icon: lucide.FolderOpen,
			OnPress: pick, TestID: testID + "-pick"})
	}
	rows := []core.Node{core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8},
		Children: children}}
	if errText != nil {
		rows = append(rows, core.Show{When: func() bool { return errText() != "" }, Then: func(*core.Scope) core.Node {
			return core.Text{ContentFunc: errText, MaxLines: 3, Style: core.Style{FontSize: 11, LineHeight: 15},
				StyleFunc: ui.Colored(colors.Danger)}
		}})
	}
	return core.Group(rows)
}

// repoPathErrText 是仓库目录那一行的错误说明。
func (m *model) repoPathErrText() string {
	if s := m.snapshot(); s.RepoPathErr != nil && s.Paths.RepoDir == "" {
		return attentionSummary(s.RepoPathErr.Error())
	}
	return ""
}

// treePathErrText 是 Work Tree 目录那一行的错误说明。
func (m *model) treePathErrText() string {
	if s := m.snapshot(); s.TreePathErr != nil {
		return attentionSummary(s.TreePathErr.Error())
	}
	return ""
}

// terminalPage 是终端页：当前选中任务的终端，断开时可见并可重连。
func (m *model) terminalPage() core.Node {
	colors := ui.ColorsOf(m.cx)
	id := m.tab.Get()
	var tab *termTab
	for _, t := range m.terms.tabs {
		if t.ID() == id {
			tab = t
			break
		}
	}
	if tab == nil {
		return core.View{Style: core.Style{FlexGrow: 1, AlignItems: core.AlignCenter, JustifyContent: core.JustifyCenter},
			Children: []core.Node{ui.Text("这个终端已经结束或不再可见", 13, ui.Regular, colors.Muted)}}
	}
	term := m.terms.ensure(m.cx, tab)
	return core.View{TestID: "terminal-page", Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Direction: core.Column, Overflow: core.Hidden},
		Children: []core.Node{
			core.View{StyleFunc: func() core.Style {
				return core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8, Padding: core.Symmetric(6, 12),
					Borders: core.Edges{Bottom: 1}, BorderColor: colors.Border(), Background: colors.Surface()}
			}, Children: []core.Node{
				ui.Text(tab.Label, 12, ui.Medium, colors.Text),
				ui.Text("地址 "+tab.Addr, 11, ui.Regular, colors.Muted),
				ui.Spacer{},
				core.Show{When: func() bool { return tab.err != nil }, Then: func(*core.Scope) core.Node {
					return core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8},
						Children: []core.Node{
							core.Text{ContentFunc: func() string { return attentionSummary(tab.err.Error()) }, MaxLines: 1,
								Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Danger)},
							ui.Button{Text: "重连", Variant: ui.ButtonOutline, Icon: lucide.RefreshCw, OnPress: func() {
								tab.retry()
								m.rev.Update(func(n int) int { return n + 1 })
							}, TestID: "reconnect"},
						}}
				}},
			}},
			core.View{Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
				Children: []core.Node{terminal.View{Terminal: term, AutoFocus: true, Style: core.Style{FlexGrow: 1},
					TestID: "terminal"}}},
		}}
}
