package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nsgo-dev/nsgo/base"
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
	// opts 是本次运行使用的选项；initialOpts 保留命令行初始入参，用于区分
	// 命令行一次性 -repo-dir 覆盖与用户在界面中的选择。
	opts        app.Options
	initialOpts app.Options

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
	// structureRev 只在节点/依赖拓扑变化时递增。flow 的 Data 订阅它，
	// 因此日志或运行状态刷新不会触发拓扑布局，拓扑变化也不会漏刷新。
	structureRev *core.Signal[int]
	// termsRev 只在终端页集合或地址变化时递增。tab 选项订阅它而不是
	// 全局 rev，避免日志刷新反复重算所有终端 tab。
	termsRev *core.Signal[int]

	// issueText 是录入的 issue 编号。
	issueText *core.Signal[string]
	// workText、repoText、treeText 是三个目录：工作目录、仓库目录、
	// worktree 目录。自动解析成功的值会预填进来；当工作目录与仓库目录
	// 不一致且用户在界面选定仓库后，会重新查找 worktree 并保存到
	// <work-dir>/fastimpl.toml，方便下次打开直接复用。
	workText, repoText, treeText *core.Signal[string]
	// repoCandidates / treeCandidates 是自动搜索在工作目录或仓库下发现的
	// 多个候选目录，供用户在启动表单直接点击选择；candidatesRev 在列表变化时递增。
	candidatesRev         *core.Signal[int]
	repoCandidates        []string
	repoCandidatesWorkDir string
	treeCandidates        []string
	treeCandidatesRepoDir string
	// pathSaveHint 展示跨目录配置自动写回 <work-dir>/fastimpl.toml 的结果。
	pathSaveHint *core.Signal[string]
	// persistSeq / persistedSeq 跟踪用户主动触发的路径预览，确保预览落定后
	// 最多写盘一次；lastDiscovered* 避免输入框无改动失焦时重复发现；
	// lastSaved* 避免同一组目录重复写盘。
	persistSeq         uint64
	persistedSeq       uint64
	persistTreeOnly    bool
	lastDiscoveredWork string
	lastDiscoveredRepo string
	lastDiscoveredTree string
	lastSavedWork      string
	lastSavedRepo      string
	lastSavedTree      string
	// settingsOpen 控制设置对话框；settings 持有配置与提示词编辑状态。
	settingsOpen *core.Signal[bool]
	settings     *settingsModel
	// tab 是当前选中的页面：pageGraph、pageLog 或某个终端页的 ID。
	tab *core.Signal[string]
	// now 是界面时钟：tooltip 里的工作时长每秒随它刷新。
	now *core.Signal[time.Time]
	// logScroll/logFollowing/logLastY 让日志页在用户位于底部时跟随
	// 新日志，在用户向上翻阅时不把它拽回底部。
	logScroll    *core.Ref
	logFollowing *core.Signal[bool]
	logLastY     core.Points
	// selected 是图上选中的节点 ID。
	selected *core.Signal[string]
	// logCopied/termCopied 与 logSaved/termSaved 在复制/保存成功后短暂为真，
	// 把按钮文字变成"已复制"/"已保存"；用模型级信号是因为页面随状态刷新重建，
	// 局部信号会丢。
	logCopied, termCopied *core.Signal[bool]
	logSaved, termSaved   *core.Signal[bool]

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

// pageGraph 是依赖图页的标识，也是 tab 的第一项；pageLog 是运行日志页。
const (
	pageGraph = "graph"
	pageLog   = "log"
)

// newModel 组装界面模型：它接管会话的事件，并在界面线程上合并刷新。
func newModel(cx *core.Scope, s *session, opts app.Options) *model {
	m := &model{
		cx: cx, session: s, opts: opts, initialOpts: opts,
		rev:           core.NewSignal(cx, 0),
		structureRev:  core.NewSignal(cx, 0),
		termsRev:      core.NewSignal(cx, 0),
		candidatesRev: core.NewSignal(cx, 0),
		issueText:     core.NewSignal(cx, ""),
		workText:      core.NewSignal(cx, ""),
		repoText:      core.NewSignal(cx, ""),
		treeText:      core.NewSignal(cx, ""),
		pathSaveHint:  core.NewSignal(cx, ""),
		settingsOpen:  core.NewSignal(cx, false),
		tab:           core.NewSignal(cx, pageGraph),
		selected:      core.NewSignal(cx, ""),
		cam:           &camera.State{},
		flowState:     &uiflow.State{},
		now:           core.NewSignal(cx, cx.WallTime()),
		logScroll:     &core.Ref{},
		logFollowing:  core.NewSignal(cx, true),
		logCopied:     core.NewSignal(cx, false),
		termCopied:    core.NewSignal(cx, false),
		logSaved:      core.NewSignal(cx, false),
		termSaved:     core.NewSignal(cx, false),
	}
	m.post = cx.Post
	// 轻量界面时钟只更新一个信号；tooltip 通过它重算工作时长。root scope
	// 销毁时 AfterFunc 链随之停止，测试里的 manual clock 也能控制它。
	var tick func()
	tick = func() {
		m.now.Set(cx.WallTime())
		cx.AfterFunc(time.Second, tick)
	}
	cx.AfterFunc(time.Second, tick)
	m.settings = newSettingsModel(cx)
	m.terms = newTerminals(s, m.notify)
	if opts.SpecIID > 0 {
		m.issueText.Set(strconv.Itoa(opts.SpecIID))
	}
	s.SetOnChange(m.notify)
	cx.Cleanup(func() {
		m.terms.closeAll()
		s.StopObserving()
	})
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
	termsChanged := m.terms.sync(snap.Status)

	m.rev.Update(func(n int) int { return n + 1 })
	if termsChanged {
		m.termsRev.Update(func(n int) int { return n + 1 })
	}
	if rebuild {
		m.structureRev.Update(func(n int) int { return n + 1 })
	}
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
	m.syncPathDiscovery(snap)
	if snap.RepoError != nil && !m.settingsOpen.GetUntracked() {
		if repo := snap.RepoError.Dir; repo != "" && m.repoText.GetUntracked() == "" {
			m.repoText.Set(repo)
		}
		m.openSettings()
	}
	// 损坏或新版 active metadata 不能静默跳过：它可能代表当前二进制不懂的
	// 运行中 worker。先变成可见的保守失败；用户检查/清理后才能继续。
	if snap.ActiveScanErr != nil && !snap.Running && !snap.Observing && !snap.AutoStarted && snap.Phase == phaseIdle {
		m.session.failLocal(fmt.Errorf("检测到无法安全理解的 active worker metadata：%w", snap.ActiveScanErr))
		m.notify()
		return
	}
	// 空 issue 启动时优先恢复本仓库 active worker：死 supervisor 的可接管
	// 现场自动继续；活 supervisor 只读观察；心跳卡住/阶段不明则保守提示。
	// 显式输入/传入 spec 时只看同一个 spec 的 active run：无关 spec 仍在跑
	// 不应劫持这次明确启动。
	activeRuns := snap.ActiveRuns
	if text := strings.TrimSpace(m.issueText.GetUntracked()); text != "" {
		iid, err := strconv.Atoi(strings.TrimPrefix(text, "#"))
		if err != nil || iid <= 0 {
			activeRuns = nil
		} else {
			filtered := make([]scheduler.ActiveTaskMeta, 0, len(activeRuns))
			for _, meta := range activeRuns {
				if meta.SpecIID == iid {
					filtered = append(filtered, meta)
				}
			}
			activeRuns = filtered
		}
	}
	// AutoStarted 只防止“带 issue 首次启动”重复触发，不能阻止 active
	// recovery：显式启动可能已在 preflight 中转入 observer，原 supervisor
	// 退出后仍必须自动评估接管。接管失败会进入 failed/stopped，不会循环。
	if len(activeRuns) > 0 && !snap.Running && !snap.Observing && snap.Phase == phaseIdle {
		choice := scheduler.ChooseActiveSpec(activeRuns, time.Now())
		if choice.SpecIID > 0 && m.issueText.GetUntracked() == "" {
			m.issueText.Set(strconv.Itoa(choice.SpecIID))
		}
		switch choice.State {
		case scheduler.ActiveRecoveryAdoptable:
			m.session.MarkAutoStarted()
			m.start()
			return
		case scheduler.ActiveRecoveryObserve:
			m.session.ObserveActive(choice.Tasks)
			return
		case scheduler.ActiveRecoveryBlocked:
			m.session.failLocal(fmt.Errorf("检测到 issue #%d 的 active worker，但 heartbeat/阶段状态不安全；请先确认原 fastimpl 进程状态或清理 metadata", choice.SpecIID))
			m.notify()
			return
		}
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
	return &liveGraph{Revision: m.rev, Now: m.now, Lookup: func(k graph.Key) (graph.Node, scheduler.TaskRuntime, bool, bool) {
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
	m.persistStartPaths(opts)
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
// 目录的只读信息栏。根视图显式声明默认箭头：依赖图和终端屏幕可以在
// 自己的命中层改用抓取/文本光标，而 Linux/GTK 离开这些层时总能回到
// 一个明确的默认光标，而不是继承上一次的特殊光标。
func (m *model) build() core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.View{TestID: "root", StyleFunc: func() core.Style {
		return core.Style{Width: core.Pct(100), Height: core.Pct(100), Direction: core.Column, Overflow: core.Hidden,
			Background: colors.Background(), Color: colors.Text(), Cursor: core.CursorDefault}
	}, Children: []core.Node{
		m.header(),
		core.View{Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinHeight: core.Pt(0), Direction: core.Column, Gap: 0, Overflow: core.Hidden},
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
		case s.Observing:
			return "只读观察中"
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
		return s.Observing || (s.Running && !s.WaitingStart)
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

// pickDir 打开系统的目录选择器，把选中的目录交给 set 处理。
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

// pickRepo 选仓库主 checkout（必须是仓库顶层），选定后重新查找 Work Tree 目录并按需保存配置。
func (m *model) pickRepo() {
	m.pickDir("选择仓库顶层目录",
		func() string { return pickStartDir(m.repoText.GetUntracked(), m.workText.GetUntracked()) },
		m.selectRepo)
}

// pickTree 选 worktree 父目录，选定后重新校验并按需保存配置。
func (m *model) pickTree() {
	m.pickDir("选择 Work Tree 目录",
		func() string { return pickStartDir(m.treeText.GetUntracked(), m.repoText.GetUntracked()) },
		m.selectTree)
}

// pickWork 选 fastimpl 工作目录，选定后按新工作目录重新发现。
func (m *model) pickWork() {
	m.pickDir("选择工作目录", m.workText.GetUntracked, m.selectWork)
}

// tabs 是图页与终端页的切换条。
func (m *model) tabs() core.Node {
	colors := ui.ColorsOf(m.cx)
	options := func() []ui.Option[string] {
		m.termsRev.Get()
		opts := []ui.Option[string]{
			{Value: pageGraph, Label: "依赖图", Icon: lucide.Workflow, TestID: "tab-graph"},
			{Value: pageLog, Label: "日志", Icon: lucide.ScrollText, TestID: "tab-log"},
		}
		for _, tab := range m.terms.tabs {
			opts = append(opts, ui.Option[string]{
				Value:     tab.ID(),
				LabelFunc: func() string { return m.terminalTabTitle(tab) },
				Content:   m.terminalTabContent(tab),
				TestID:    "terminal-tab-" + tab.ID(),
			})
		}
		return opts
	}
	selectedTab := &core.Style{Color: colors.Accent()}
	return core.View{TestID: "tabs", StyleFunc: func() core.Style {
		return core.Style{Padding: core.Symmetric(2, 10), Borders: core.Edges{Bottom: 1}, BorderColor: colors.Border(),
			Background: colors.Surface()}
	}, Children: []core.Node{
		ui.Tabs[string]{Variant: ui.TabsUnderline, OptionsFunc: options, Value: m.tab.Get, OnChange: m.tab.Set, Size: ui.SizeSmall,
			TabStyle: core.Style{Padding: core.Symmetric(7, 12), FontWeight: ui.Medium, Selected: selectedTab},
			Label:    "页面"},
	}}
}

// body 是当前页。
func (m *model) body() core.Node {
	return core.View{Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
		Children: []core.Node{core.Show{
			When: func() bool { return m.tab.Get() == pageGraph },
			Then: func(cx *core.Scope) core.Node { return m.graphPage() },
			Else: func(cx *core.Scope) core.Node {
				return core.Show{
					When: func() bool { return m.tab.Get() == pageLog },
					Then: func(cx *core.Scope) core.Node { return m.logPage() },
					Else: func(cx *core.Scope) core.Node {
						// 终端 A → 终端 B 时外层两个 Show 的条件都仍然是
						// false，不会重建分支；这里必须让 terminalPage 跟着
						// 当前 tab 动态求值，否则界面会停留在第一次打开的终端。
						return core.Dynamic(func(cx *core.Scope) core.Node { return m.terminalPage() })
					},
				}
			},
		}}}
}

// terminalRuntime 读取一个终端页对应的运行数据；读取 rev，因此调用它的
// 标签/tooltip 会随 scheduler 状态刷新。
func (m *model) terminalRuntime(key graph.Key) (scheduler.TaskRuntime, bool) {
	m.rev.Get()
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.runtime[key]
	return rt, ok
}

// terminalTabTitle 是用户确认的终端页标题：issue 号 + 开始时分秒。
// 集成分支 agent 没有任务 issue，因此保留“集成”前缀以区别于普通 worker。
func (m *model) terminalTabTitle(tab *termTab) string {
	base := fmt.Sprintf("#%d", tab.Key.IID)
	if tab.Intg {
		base = "集成 " + base
	}
	rt, ok := m.terminalRuntime(tab.Key)
	if !ok || rt.StartedAt.IsZero() {
		return base
	}
	return base + " · " + rt.StartedAt.Format("15:04:05")
}

// terminalTabContent 是终端 tab 的自定义内容：视觉上是图标与动态标题，
// hover/键盘聚焦时显示动态详细信息 tooltip。
func (m *model) terminalTabContent(tab *termTab) core.Node {
	trigger := core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 6},
		Children: []core.Node{
			ui.Icon(lucide.SquareTerminal, 14, ui.Inherited),
			core.Text{ContentFunc: func() string { return m.terminalTabTitle(tab) }, MaxLines: 1},
		}}
	if tab.tipOpen == nil {
		tab.tipOpen = core.NewSignal(m.cx, false)
	}
	open := tab.tipOpen
	return ui.Tooltip{
		TextFunc: func() string {
			if !open.Get() {
				return m.terminalTooltipSummary(tab)
			}
			return m.terminalTooltip(tab)
		},
		OnOpenChange: func(opened bool, _ base.Reason) { open.Set(opened) },
		Trigger:      trigger,
		TestID:       "terminal-tooltip-" + tab.ID(),
	}
}

// terminalTooltipSummary 是关闭状态下的静态辅助文本。NSGo 会让关闭的
// tooltip 隐藏挂载来描述 trigger；这里不能读 now/rev，否则每个终端的
// 隐藏文本都会每秒刷新，终端多时白白占用 UI 线程。
func (m *model) terminalTooltipSummary(tab *termTab) string {
	kind := "任务 worker"
	if tab.Intg {
		kind = "集成分支 agent"
	}
	lines := []string{
		"类型：" + kind,
		fmt.Sprintf("Issue：#%d", tab.Key.IID),
	}
	if tab.Label != "" {
		lines = append(lines, "hc 终端："+tab.Label)
	}
	lines = append(lines, "打开 tooltip 后显示开始时间、用时与连接详情")
	return strings.Join(lines, "\n")
}

// terminalTooltip 组出一个终端的详细信息。所有字段都来自快照副本；
// 运行中时长读取界面时钟信号，因此 tooltip 打开时也会每秒更新。
func (m *model) terminalTooltip(tab *termTab) string {
	now := m.now.Get()
	m.rev.Get()
	m.mu.Lock()
	rt, rtOK := m.runtime[tab.Key]
	node, nodeOK := m.nodes[tab.Key]
	spec := m.snap.Spec
	m.mu.Unlock()

	title := ""
	if nodeOK {
		title = node.Title
	}
	if tab.Intg && tab.Key == spec.Key {
		title = spec.Title
	}
	if title == "" {
		title = "（无标题）"
	}
	if !rtOK {
		rt = scheduler.TaskRuntime{Key: tab.Key, Addr: tab.Addr, Label: tab.Label, Integration: tab.Intg}
	}
	kind := "任务 worker"
	if tab.Intg {
		kind = "集成分支 agent"
	}
	lines := []string{
		"类型：" + kind,
		fmt.Sprintf("Issue：#%d", tab.Key.IID),
		"标题：" + title,
		"开始时间：" + formatLogTime(rt.StartedAt),
		"已工作时间：" + terminalWorkTime(rt, now),
	}
	if !rt.FinishedAt.IsZero() {
		lines = append(lines, "结束时间："+formatLogTime(rt.FinishedAt))
	}
	label := rt.Label
	if label == "" {
		label = tab.Label
	}
	addr := rt.Addr
	if addr == "" {
		addr = tab.Addr
	}
	if label != "" {
		lines = append(lines, "hc 终端："+label)
	}
	if addr != "" {
		lines = append(lines, "hc 地址："+addr)
	}
	if label != "" {
		lines = append(lines, "连接：hc attach "+label)
	}
	if rt.Accepted != "" {
		commit := rt.Accepted
		if len(commit) > 12 {
			commit = commit[:12]
		}
		lines = append(lines, "已接受提交："+commit)
	}
	if rt.Attention != "" {
		lines = append(lines, "需要注意："+attentionSummary(rt.Attention))
	}
	return strings.Join(lines, "\n")
}

// formatLogTime 是 tooltip 里完整时间的稳定格式。
func formatLogTime(t time.Time) string {
	if t.IsZero() {
		return "未开始"
	}
	return t.Format("2006-01-02 15:04:05")
}

// terminalWorkTime 计算工作时长：运行中算到界面时钟，结束后固定到
// FinishedAt。时钟万一倒退则按 0 秒显示，不出负数。
func terminalWorkTime(rt scheduler.TaskRuntime, now time.Time) string {
	if rt.StartedAt.IsZero() {
		return "未开始"
	}
	end := now
	if !rt.FinishedAt.IsZero() {
		end = rt.FinishedAt
	}
	if end.Before(rt.StartedAt) {
		end = rt.StartedAt
	}
	return formatWorkDuration(end.Sub(rt.StartedAt))
}

// formatWorkDuration 用中文短格式展示秒级时长。
func formatWorkDuration(d time.Duration) string {
	seconds := max(int(d/time.Second), 0)
	hours := seconds / 3600
	minutes := seconds % 3600 / 60
	seconds %= 60
	switch {
	case hours > 0:
		return fmt.Sprintf("%d小时%d分%d秒", hours, minutes, seconds)
	case minutes > 0:
		return fmt.Sprintf("%d分%d秒", minutes, seconds)
	default:
		return fmt.Sprintf("%d秒", seconds)
	}
}

// logScrolled 跟踪用户是否留在日志底部：向上翻阅暂停跟随，回到底部恢复。
func (m *model) logScrolled(e core.ScrollEvent) {
	end := e.Offset.Y+e.Size.Height >= e.ContentSize.Height-2
	switch {
	case end:
		m.logFollowing.Set(true)
	case e.Offset.Y < m.logLastY-1:
		m.logFollowing.Set(false)
	}
	m.logLastY = e.Offset.Y
}

// logGrown 在日志内容高度变化后保持底部跟随。
func (m *model) logGrown(core.Bounds) {
	if m.logFollowing.GetUntracked() {
		m.logScroll.ScrollToEnd(false)
	}
}

// flashFeedback 把操作反馈信号置真，1.5 秒后恢复按钮文字；AfterFunc 随
// scope 销毁停止，测试里的 manual clock 也能控制它。
func (m *model) flashFeedback(sig *core.Signal[bool]) {
	sig.Set(true)
	m.cx.AfterFunc(1500*time.Millisecond, func() { sig.Set(false) })
}

// copyButton 是日志页与终端页共用的复制按钮：按下时把 text() 写进系统
// 剪贴板，按钮文字短暂变成"已复制"。text 返回空时不动作——复制全部
// 文本是显式按钮而非快捷键，不能把空内容悄悄刷上剪贴板。
func (m *model) copyButton(sig *core.Signal[bool], label string, text func() string, disabled func() bool, testID string) ui.Button {
	return ui.Button{
		TextFunc: func() string {
			if sig.Get() {
				return "已复制"
			}
			return label
		},
		Variant:  ui.ButtonOutline,
		Icon:     lucide.Copy,
		Disabled: disabled,
		TestID:   testID,
		OnPress: func() {
			t := text()
			if t == "" {
				return
			}
			m.cx.SetClipboard(t)
			m.flashFeedback(sig)
		},
	}
}

// exportName 是导出文件的默认名：时间戳避免连续保存时默认覆盖同一份文件。
func (m *model) exportName(prefix, ext string) string {
	return fmt.Sprintf("%s-%s.%s", prefix, m.cx.WallTime().Format("20060102-150405"), ext)
}

// saveButton 是日志页与终端页共用的保存按钮：按下时经系统保存对话框
// 把 text() 写到用户选择的位置。保存是显式导出，空内容时不打开对话框；
// 成功后按钮文字短暂变成"已保存"，写入失败用系统提示说明原因。
func (m *model) saveButton(sig *core.Signal[bool], name func() string, text func() string, disabled func() bool, testID string) ui.Button {
	return ui.Button{
		TextFunc: func() string {
			if sig.Get() {
				return "已保存"
			}
			return "保存"
		},
		Variant:  ui.ButtonOutline,
		Icon:     lucide.Save,
		Disabled: disabled,
		TestID:   testID,
		OnPress: func() {
			t := text()
			if t == "" {
				return
			}
			m.cx.SaveFile(core.SaveOptions{Name: name(), Start: m.settingsWorkDir(), Data: []byte(t)}, func(saved bool, _ string, err error) {
				if err != nil {
					m.cx.Post(func() {
						m.cx.ShowAlert(core.AlertConfig{Title: "保存失败", Message: err.Error(), Cancelable: true,
							Actions: []core.AlertAction{{Label: "确定"}}}, nil)
					})
					return
				}
				if saved {
					m.cx.Post(func() { m.flashFeedback(sig) })
				}
			})
		},
	}
}

// logPage 是运行日志页。日志正文用等宽字体整块展示；快照最多保留最近
// logLines 行，持久文件是否启用由设置里的 log_file 决定。
func (m *model) logPage() core.Node {
	colors := ui.ColorsOf(m.cx)
	logBar := ui.ScrollbarTheme(m.cx)
	logBar.Mode = base.ScrollbarAlways
	logBar.Vertical.Label = "运行日志滚动条"
	return core.View{TestID: "log-page", Style: core.Style{FlexGrow: 1, FlexShrink: 1, MinHeight: core.Pt(0), Direction: core.Column, Overflow: core.Hidden},
		Children: []core.Node{
			core.View{StyleFunc: func() core.Style {
				return core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8, Padding: core.Symmetric(7, 12),
					Borders: core.Edges{Bottom: 1}, BorderColor: colors.Border(), Background: colors.Surface()}
			}, Children: []core.Node{
				ui.Text("运行日志", 12, ui.Medium, colors.Text),
				core.Text{ContentFunc: func() string { return fmt.Sprintf("最近 %d 行", len(m.snapshot().Log)) },
					MaxLines: 1, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted), TestID: "log-count"},
				ui.Spacer{},
				core.Text{Content: "在底部时自动跟随新日志；向上翻阅会暂停跟随", MaxLines: 1,
					Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)},
				m.copyButton(m.logCopied, "复制", func() string {
					return strings.Join(m.snapshot().Log, "\n")
				}, func() bool { return len(m.snapshot().Log) == 0 }, "copy-log"),
				m.saveButton(m.logSaved, func() string { return m.exportName("fastimpl-log", "log") }, func() string {
					return strings.Join(m.snapshot().Log, "\n")
				}, func() bool { return len(m.snapshot().Log) == 0 }, "save-log"),
			}},
			core.Scroll{Ref: m.logScroll, OnScroll: m.logScrolled, Bar: logBar, TestID: "log-scroll",
				Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0)},
				Children: []core.Node{core.View{OnLayout: m.logGrown,
					Style: core.Style{Padding: core.All(12), MinWidth: core.Pct(100)},
					Children: []core.Node{core.Text{ContentFunc: func() string {
						lines := m.snapshot().Log
						if len(lines) == 0 {
							return "暂无运行日志"
						}
						return strings.Join(lines, "\n")
					}, Style: ui.TypeCode.Merge(core.Style{MinWidth: core.Pct(100)}), StyleFunc: ui.Colored(colors.Text), TestID: "log-lines"}}},
				}},
		}}
}

// graphPage 是依赖图页：可平移缩放的画布、缩放工具条与图例。
func (m *model) graphPage() core.Node {
	return core.View{TestID: "graph-page", Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
		Children: []core.Node{
			core.Show{When: func() bool {
				s := m.snapshot()
				return s.GraphReady || s.Phase == phaseObserving
			}, Then: func(*core.Scope) core.Node {
				if m.snapshot().Phase == phaseObserving {
					return m.observerPage()
				}
				look := m.look()
				return core.View{Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
					Children: []core.Node{
						uiflow.View{Camera: m.cam, State: m.flowState,
							Data: func() uiflow.Data {
								// 只有结构变了才换数据；节点状态的刷新走卡片
								// 自己订阅的信号，不动布局与相机。
								m.structureRev.Get()
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
				if m.snapshot().Phase == phaseObserving {
					return m.observerPage()
				}
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
		item(lucide.CircleDashed, pendingColor(colors), "等待前置"),
		item(lucide.CirclePlay, colors.Up, "可开始"),
		item(lucide.LoaderCircle, colors.Accent, "运行中"),
		item(lucide.GitMerge, colors.Accent, "集成中"),
		item(lucide.CircleCheck, colors.Up, "已集成"),
		item(lucide.CircleCheck, colors.Muted, "已关闭"),
		item(lucide.TriangleAlert, colors.Warn, "需要处理"),
		item(lucide.CloudCog, colors.Info, "远端进行中"),
	}}
}

// observerPage 是 active worker 只读观察页。依赖图属于原 supervisor 的
// 内存状态，本进程不联网重建、不发 claim；可安全展示的是 metadata 里的
// issue、worktree、hc 终端与 supervisor/heartbeat 信息，终端页在上方 tab 中。
func (m *model) observerPage() core.Node {
	colors := ui.ColorsOf(m.cx)
	runView := func(meta scheduler.ActiveTaskMeta) core.Node {
		current := func(render func(scheduler.ActiveTaskMeta) string) func() string {
			return func() string {
				for _, candidate := range m.snapshot().ActiveRuns {
					if candidate.IssueIID == meta.IssueIID {
						return render(candidate)
					}
				}
				return render(meta)
			}
		}
		heartbeatText := func(meta scheduler.ActiveTaskMeta) string {
			heartbeat := "未知"
			if !meta.Supervisor.HeartbeatAt.IsZero() {
				heartbeat = meta.Supervisor.HeartbeatAt.Format("2006-01-02 15:04:05")
			}
			return fmt.Sprintf("supervisor pid: %d · instance: %s · heartbeat: %s", meta.Supervisor.PID, meta.Supervisor.InstanceID, heartbeat)
		}
		return core.View{TestID: fmt.Sprintf("observer-run-%d", meta.IssueIID),
			Style: core.Style{Direction: core.Column, Gap: 3, Padding: core.All(8), BorderWidth: 1, CornerRadius: 8},
			StyleFunc: func() core.Style {
				return core.Style{BorderColor: colors.Border(), Background: colors.Background()}
			}, Children: []core.Node{
				core.Text{ContentFunc: current(func(meta scheduler.ActiveTaskMeta) string {
					return fmt.Sprintf("issue #%d · stage %s · hc terminal: %s (%s)", meta.IssueIID, meta.Stage, meta.TerminalName, meta.TerminalAddr)
				}), MaxLines: 2, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Text)},
				core.Text{ContentFunc: current(func(meta scheduler.ActiveTaskMeta) string { return "worktree: " + meta.Worktree }), MaxLines: 2,
					Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)},
				core.Text{ContentFunc: current(heartbeatText), MaxLines: 2, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)},
			}}
	}
	children := []core.Node{
		core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 10}, Children: []core.Node{
			ui.Icon(lucide.Eye, 24, colors.Accent),
			ui.Text("正在只读观察 active worker", 16, ui.Semibold, colors.Text),
		}},
		core.Text{ContentFunc: func() string { return m.snapshot().Message }, MaxLines: 3,
			Style: ui.TypeBody, StyleFunc: ui.Colored(colors.Muted)},
		core.Text{ContentFunc: func() string {
			s := m.snapshot()
			return fmt.Sprintf("已发现 %d 个可观察终端；打开上方终端 tab 查看，键盘输入已被禁用", len(s.Status))
		}, MaxLines: 2, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted), TestID: "observer-summary"},
	}
	for _, meta := range m.snapshot().ActiveRuns {
		children = append(children, runView(meta))
	}
	children = append(children, core.Text{Content: "原 fastimpl 进程退出且现场安全后，本观察者会自动评估并接管；当前实例不会发送 prompt、claim、merge、close 或 kill。",
		MaxLines: 4, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)})
	return core.View{TestID: "observer-page", Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), AlignItems: core.AlignCenter,
		JustifyContent: core.JustifyCenter, Padding: core.All(28)},
		Children: []core.Node{
			core.View{Style: core.Style{Width: core.Pt(620), Padding: core.All(24), Gap: 12, BorderWidth: 1, CornerRadius: 14},
				StyleFunc: func() core.Style {
					return core.Style{Background: colors.Surface(), BorderColor: colors.Border()}
				}, Children: children},
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
		case s.RepoPathErr != nil && len(s.RepoCandidates) > 0:
			return "工作目录附近检测到多个仓库，请在上方列表中选择一个仓库目录"
		case s.RepoPathErr != nil:
			return "仓库目录无法自动解析，请填写或选择仓库顶层目录"
		case s.TreePathErr != nil && len(s.TreeCandidates) > 0:
			return "检测到多个 Work Tree 目录，请在上方列表中选择一个"
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
					m.formRow("issue", "spec issue 编号", m.issueText.Get, m.issueText.Set, nil, nil, nil, "form-issue"),
					m.formRow("工作目录", "fastimpl 运行目录", m.workText.Get, m.workText.Set, m.commitWorkInput, m.pickWork, nil, "form-work"),
					m.formRow("仓库目录", "目标仓库顶层目录", m.repoText.Get, m.repoText.Set, m.commitRepoInput, m.pickRepo, m.repoPathErrText, "form-repo"),
					m.repoCandidatesSection(),
					m.formRow("Work Tree 目录", "worktree 父目录", m.treeText.Get, m.treeText.Set, m.commitTreeInput, m.pickTree, m.treePathErrText, "form-tree"),
					m.treeCandidatesSection(),
					core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 10}, Children: []core.Node{
						ui.Spacer{},
						ui.Button{Text: "构建依赖图", Variant: ui.ButtonPrimary, Icon: lucide.Workflow, OnPress: m.start, TestID: "empty-start"},
					}},
					core.Text{ContentFunc: hint, MaxLines: 3, Style: ui.TypeCaption.Merge(core.Style{TextAlign: core.TextAlignCenter}),
						StyleFunc: ui.Colored(colors.Muted), TestID: "form-hint"},
					m.pathSaveHintSection(),
				}},
		}}
}

// formRow 是启动表单里的一行：标签、输入框，可选的目录选择按钮，以及
// 该行自己的错误说明。pick 为 nil 时没有选择按钮（issue 编号用）；
// onCommit 在回车或失焦时触发，用于手输路径后重新查找。
func (m *model) formRow(label, placeholder string, value func() string, set func(string), onCommit func(), pick func(), errText func() string, testID string) core.Node {
	colors := ui.ColorsOf(m.cx)
	input := ui.Input{Value: value, OnChange: set, Placeholder: placeholder, TestID: testID}
	if onCommit != nil {
		input.OnSubmit = func(string) { onCommit() }
		input.OnBlur = onCommit
	}
	children := []core.Node{
		ui.Text(label, 11, ui.Medium, colors.Muted),
		core.View{Style: core.Style{FlexGrow: 1, MinWidth: core.Pt(0)}, Children: []core.Node{input}},
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

// repoPathErrText 是仓库目录那一行的错误说明；多仓库歧义时下方已有候选列表，
// 这里不重复倾倒冗长的逗号分隔路径报错。
func (m *model) repoPathErrText() string {
	if s := m.snapshot(); s.RepoPathErr != nil && s.Paths.RepoDir == "" && len(s.RepoCandidates) == 0 {
		return attentionSummary(s.RepoPathErr.Error())
	}
	return ""
}

// treePathErrText 是 Work Tree 目录那一行的错误说明；多候选时下方已有选择列表。
func (m *model) treePathErrText() string {
	if s := m.snapshot(); s.TreePathErr != nil && len(s.TreeCandidates) == 0 {
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
				m.copyButton(m.termCopied, "复制全部", func() string {
					if tab.term == nil {
						return ""
					}
					return strings.TrimRight(tab.term.Text(), " \t\n")
				}, func() bool { return tab.term == nil }, "copy-terminal"),
				m.saveButton(m.termSaved, func() string { return m.exportName("fastimpl-terminal", "md") }, func() string {
					if tab.term == nil {
						return ""
					}
					return strings.TrimRight(tab.term.Text(), " \t\n")
				}, func() bool { return tab.term == nil }, "save-terminal"),
			}},
			core.View{Style: core.Style{FlexGrow: 1, MinHeight: core.Pt(0), Overflow: core.Hidden},
				Children: []core.Node{terminal.View{Terminal: term, AutoFocus: true, Style: core.Style{FlexGrow: 1},
					TestID: "terminal"}}},
		}}
}
