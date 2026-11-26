package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// refreshInterval 是本地快照的合并刷新间隔。它只读内存里的图与
// scheduler 状态，绝不轮询 forge；结果到达时另有一次强制刷新。
const refreshInterval = 400 * time.Millisecond

// logLines 是界面保留的运行日志行数。
const logLines = 200

// logFileReadBytes 是从持久日志文件读回展示内容的上限。它保护 GUI
// 不被长期累积的大文件拖慢；最终仍只保留最近 logLines 行。
const logFileReadBytes = 64 * 1024

// observePollIntervalNanos 是只读观察模式检查原 supervisor 是否退出的间隔。
// 它只读本机 metadata，不访问 forge/hc 控制面；atomic 允许测试安全缩短。
var observePollIntervalNanos atomic.Int64

func observePollInterval() time.Duration {
	if nanos := observePollIntervalNanos.Load(); nanos > 0 {
		return time.Duration(nanos)
	}
	return 5 * time.Second
}

// phase 是一次运行在界面上的阶段。
type phase int

const (
	// phaseIdle 是还没开始（空状态）。
	phaseIdle phase = iota
	// phaseStarting 是加载配置、解析仓库。
	phaseStarting
	// phaseBuilding 是建依赖图。
	phaseBuilding
	// phaseReady 是依赖图已建好，正在等用户点击开始调度。
	phaseReady
	// phasePreparing 是准备集成分支、MR 与 hc 连接。
	phasePreparing
	// phaseScheduling 是调度器在跑。
	phaseScheduling
	// phaseDone 是全部任务已集成。
	phaseDone
	// phaseStopped 是用户停止，调度器已收尾。
	phaseStopped
	// phaseObserving 是只读观察另一个仍活着的 fastimpl supervisor。
	phaseObserving
	// phaseFailed 是运行失败（含被取消以外的错误）。
	phaseFailed
)

// String 返回阶段的稳定名称，供界面与测试使用。
func (p phase) String() string {
	switch p {
	case phaseStarting:
		return "starting"
	case phaseBuilding:
		return "building"
	case phaseReady:
		return "ready"
	case phasePreparing:
		return "preparing"
	case phaseScheduling:
		return "scheduling"
	case phaseDone:
		return "done"
	case phaseStopped:
		return "stopped"
	case phaseObserving:
		return "observing"
	case phaseFailed:
		return "failed"
	}
	return "idle"
}

// specView 是父 issue 的展示信息。
type specView struct {
	Key    graph.Key
	Title  string
	Open   bool
	Number int
	Deps   []graph.Key
}

// snapshot 是界面读到的一份运行状态。它是纯数据，复制后才交给界面，
// 所以界面线程读它不会碰到任何锁。
type snapshot struct {
	Revision  int
	Phase     phase
	Issue     int
	Progress  graph.BuildProgress
	Message   string
	Err       error
	RepoError *app.RepositoryError
	Spec      specView
	Nodes     []graph.Node
	Status    []scheduler.TaskRuntime
	Log       []string
	// GraphReady 表示依赖图已经建好、可以画了。
	GraphReady bool
	// Ready 是当前可调度、可开始工作的节点。
	Ready []graph.Key
	// WaitingStart 表示管线停在调度门禁前，等待用户点击开始。
	WaitingStart bool
	// Running 表示这次运行还在进行，停止按钮因此有意义。
	Running bool
	// Paths 是三个目录的预览结果：能解析的都填好，失败的那一项在
	// RepoPathErr/TreePathErr 里说明。全部就绪时 PathsReady 为真。
	Paths      app.Paths
	PathsReady bool
	// PathErr 是工作目录或配置本身不可用（普通失败，不是仓库提示）。
	PathErr                  error
	RepoPathErr, TreePathErr error
	// AutoStarted 表示带 issue 启动的那次自动开始已经发生，不会重复。
	AutoStarted bool
	// ActiveRuns 是目录预览时扫描到的本仓库 active worker metadata。
	ActiveRuns []scheduler.ActiveTaskMeta
	// ActiveScanErr 表示存在匹配但无法解析/版本不符的 active metadata；
	// 它不是目录错误，却必须阻止静默自动启动。
	ActiveScanErr error
	// Observing 表示当前只是只读观察另一个 live supervisor，不在本地调度。
	Observing bool
}

// session 拥有一次 GUI 运行：它把 app.RunObserved 的回调收进状态，
// 用本地合并刷新把 scheduler 快照同步进状态，并把变化通知界面。
// 它不依赖任何 UI 包，因此可以单独测试。
type session struct {
	mu       sync.Mutex
	running  bool
	phase    phase
	opts     app.Options
	prog     graph.BuildProgress
	message  string
	err      error
	repoErr  *app.RepositoryError
	spec     specView
	nodes    []graph.Node
	ready    []graph.Key
	status   []scheduler.TaskRuntime
	log      []string
	writer   *lineWriter
	g        *graph.Graph
	sched    *scheduler.Scheduler
	attach   app.AttachTerminal
	cancel   context.CancelFunc
	rev      int
	onChange func()
	// startCh 与 waitingStart 是调度门禁：依赖图建好后运行 goroutine
	// 在 startCh 上等用户确认；确认前不准备集成、不连接 hcgo。
	startCh      chan struct{}
	waitingStart bool
	// runFn 是这次运行的主体，测试用它替换 app.RunObserved；nil 表示
	// 用 app.RunObserved。
	runFn func(context.Context, app.Options, io.Writer, app.Observer) error
	// previewFn 是目录预览，测试用它替换 app.PreviewPaths；nil 表示用
	// app.PreviewPaths。
	previewFn func(context.Context, app.Options) (app.PathsPreview, error)
	// 目录预览结果：能解析的都已填好，缺的那一项在对应 Err 里。
	paths                    app.Paths
	pathsReady               bool
	pathErr                  error
	repoPathErr, treePathErr error
	discovering              bool
	// autoStarted 表示带 issue 启动时的那次自动开始已经发生。
	autoStarted bool
	// activeRuns 是启动扫描发现的 metadata；observing 为真时本进程
	// 不调度、不偷锁、不写 prompt，只把原 supervisor 的终端摆出来观察。
	activeRuns    []scheduler.ActiveTaskMeta
	activeScanErr error
	observing     bool
	// observeCancel 停止 active worker 观察者的本地 metadata 轮询。
	observeCancel context.CancelFunc
}

// newSession 创建空状态。
func newSession() *session {
	return &session{phase: phaseIdle}
}

// SetOnChange 装上变化通知。它可能在任何 goroutine 上被调用，
// 实现方必须立即返回（通常是往 UI 队列投一个合并后的更新）。
func (s *session) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Running 报告是否有运行在进行。
func (s *session) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Snapshot 返回一份可以随意读的副本。
func (s *session) Snapshot() snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshot{
		Revision:      s.rev,
		Phase:         s.phase,
		Issue:         s.opts.SpecIID,
		Progress:      s.prog,
		Message:       s.message,
		Err:           s.err,
		RepoError:     s.repoErr,
		Spec:          s.spec,
		Nodes:         append([]graph.Node(nil), s.nodes...),
		Status:        append([]scheduler.TaskRuntime(nil), s.status...),
		Log:           append([]string(nil), s.log...),
		GraphReady:    s.g != nil,
		Ready:         append([]graph.Key(nil), s.ready...),
		WaitingStart:  s.waitingStart,
		Running:       s.running,
		Paths:         s.paths,
		PathsReady:    s.pathsReady,
		PathErr:       s.pathErr,
		RepoPathErr:   s.repoPathErr,
		TreePathErr:   s.treePathErr,
		AutoStarted:   s.autoStarted,
		ActiveRuns:    append([]scheduler.ActiveTaskMeta(nil), s.activeRuns...),
		ActiveScanErr: s.activeScanErr,
		Observing:     s.observing,
	}
}

// Discover 解析三个目录（工作目录、仓库目录、worktree 目录），只读本地
// 文件系统与 git，不发任何网络请求、不写配置。能解析的都填好，缺的那
// 一项由界面补填。OnDone 在解析完成（状态已更新）后调用。
func (s *session) Discover(opts app.Options) {
	s.mu.Lock()
	if s.discovering {
		s.mu.Unlock()
		return
	}
	s.discovering = true
	s.opts = opts
	s.mu.Unlock()
	go func() {
		ctx := context.Background()
		preview, err := s.preview(ctx, opts)
		var active []scheduler.ActiveTaskMeta
		var activeScanErr error
		if err == nil && preview.Paths.RepoDir != "" {
			// active metadata 是本机 /tmp 里的原子 JSON。有效文件照常展示；
			// 但匹配却不能解析/版本不符的文件必须显式进入状态，防止 GUI
			// 把一个自己不懂的新版/损坏现场当成“没有 active run”。
			scanned, invalid, scanErr := scheduler.ScanActiveTasksDetailed(preview.Paths.RepoDir)
			if scanErr == nil {
				active = scanned
				if len(invalid) > 0 {
					activeScanErr = fmt.Errorf("invalid or unsupported active fastimpl metadata exists (%s); inspect it manually before taking over", strings.Join(invalid, ", "))
				}
			} else {
				activeScanErr = scanErr
			}
		}
		s.mu.Lock()
		s.discovering = false
		s.paths, s.pathErr = preview.Paths, err
		s.activeRuns = active
		s.activeScanErr = activeScanErr
		s.repoPathErr, s.treePathErr = preview.RepoErr, preview.TreeErr
		s.pathsReady = err == nil && preview.Ready()
		onChange := s.onChange
		s.rev++
		s.mu.Unlock()
		if onChange != nil {
			onChange()
		}
	}()
}

// preview 跑目录预览，测试可替换。
func (s *session) preview(ctx context.Context, opts app.Options) (app.PathsPreview, error) {
	s.mu.Lock()
	fn := s.previewFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, opts)
	}
	return app.PreviewPaths(ctx, opts)
}

// MarkAutoStarted 记下带 issue 启动时的自动开始已经发生。
func (s *session) MarkAutoStarted() {
	s.mu.Lock()
	s.autoStarted = true
	s.mu.Unlock()
}

// StopObserving 停止观察者的本地轮询，不改变可见状态。窗口销毁时调用，
// 避免观察 goroutine 在进程退出前多做无意义 IO；它不影响 hc worker。
func (s *session) StopObserving() {
	s.mu.Lock()
	cancel := s.observeCancel
	s.observeCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Observing 报告当前是否处于只读观察模式。
func (s *session) Observing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observing
}

// observedStatus 把 active metadata 映射成只读终端页快照。它只使用
// metadata 中已有身份，不访问 hc 控制面；空 addr 的任务只能在观察页卡片
// 中展示，不能伪造一个可连接终端。
func observedStatus(metas []scheduler.ActiveTaskMeta) []scheduler.TaskRuntime {
	status := make([]scheduler.TaskRuntime, 0, len(metas))
	for _, meta := range metas {
		if meta.TerminalAddr == "" {
			continue
		}
		status = append(status, scheduler.TaskRuntime{
			Key:       graph.Key{ProjectID: meta.ProjectID, IID: meta.IssueIID},
			Addr:      meta.TerminalAddr,
			Label:     meta.TerminalName,
			StartedAt: meta.StartedAt,
			Attention: "由另一个 fastimpl 实例只读观察中",
		})
	}
	return status
}

// ObserveActive 进入只读观察模式。它绝不启动 scheduler、不发 claim、不写
// prompt、不偷 lock；只把 metadata 里的 hc 终端身份映射成终端页，attach
// 使用 metadata 中保存的 hc binary/socket。终端连接在 terminal 层禁写，
// 因此用户也不能通过观察页向原 worker 送按键。
func (s *session) ObserveActive(metas []scheduler.ActiveTaskMeta) {
	if len(metas) == 0 {
		return
	}
	spec := metas[0].SpecIID
	status := observedStatus(metas)
	lines := make([]string, 0, len(metas)+1)
	for _, meta := range metas {
		if meta.TerminalAddr == "" {
			continue
		}
		lines = append(lines, "observing active worker issue="+strconv.Itoa(meta.IssueIID)+
			" terminal="+meta.TerminalName+" supervisor_pid="+strconv.Itoa(meta.Supervisor.PID))
	}
	if len(lines) == 0 {
		lines = append(lines, "active metadata has no attachable hc terminal")
	}
	first := metas[0]
	s.mu.Lock()
	s.phase = phaseObserving
	s.observing = true
	s.running = false
	s.opts.SpecIID = spec
	s.activeRuns = append([]scheduler.ActiveTaskMeta(nil), metas...)
	s.message = "检测到另一个 fastimpl 实例仍在管理该任务；当前只读观察"
	s.err = nil
	s.repoErr = nil
	s.status = status
	s.log = lines
	s.attach = app.ViewerAttach(first.HCBinary, first.HCSocket)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	if s.observeCancel != nil {
		s.observeCancel()
	}
	s.observeCancel = cancelWatch
	s.rev++
	onChange := s.onChange
	s.mu.Unlock()
	go s.watchObserved(watchCtx, append([]scheduler.ActiveTaskMeta(nil), metas...))
	if onChange != nil {
		onChange()
	}
}

// watchObserved 只读本机 active metadata，等原 supervisor 退出。它活着时
// 什么也不做；死亡且现场可接管时退出观察模式并回到 idle，界面既有的
// ActiveRuns 自动恢复逻辑会立刻启动本地 scheduler。metadata 消失通常表示
// 原实例已完成或另一个实例已接管，此时只回到空状态，不猜。
func (s *session) watchObserved(ctx context.Context, original []scheduler.ActiveTaskMeta) {
	if len(original) == 0 {
		return
	}
	ticker := time.NewTicker(observePollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.checkObserved(original) {
				return
			}
		}
	}
}

// checkObserved 评估一次观察对象；返回 true 表示观察模式已结束。
func (s *session) checkObserved(original []scheduler.ActiveTaskMeta) bool {
	repoDir := original[0].RepoDir
	scanned, invalid, err := scheduler.ScanActiveTasksDetailed(repoDir)
	if err != nil {
		return false
	}
	invalidMetadata := len(invalid) > 0
	byIssue := make(map[int]scheduler.ActiveTaskMeta, len(scanned))
	for _, meta := range scanned {
		if meta.SpecIID == original[0].SpecIID {
			byIssue[meta.IssueIID] = meta
		}
	}
	// 观察对象是同一个 spec 的整组 metadata，而不是启动瞬间已有的 issue
	// 集合：原 supervisor 还活着时可能继续派发后续并行任务。任务完成后
	// 会单独删除自己的 metadata；只有整组都消失时才结束观察。
	current := make([]scheduler.ActiveTaskMeta, 0, len(byIssue))
	for _, meta := range byIssue {
		current = append(current, meta)
	}
	sort.Slice(current, func(i, j int) bool { return current[i].IssueIID < current[j].IssueIID })
	blocked := invalidMetadata
	observedAlive := false
	now := time.Now()
	for _, meta := range current {
		switch meta.RecoveryState(now) {
		case scheduler.ActiveRecoveryObserve:
			observedAlive = true
		case scheduler.ActiveRecoveryBlocked:
			blocked = true
		}
	}
	if observedAlive {
		// 原 supervisor 仍活着：继续只读观察。metadata 有变化时刷进快照；
		// 没变化绝不能重复通知，否则高频轮询会用无意义 revision 饿死 UI
		// 事件循环（race/测试里的毫秒级轮询已经验证过这种饥饿）。
		s.mu.Lock()
		if !s.observing {
			s.mu.Unlock()
			return true
		}
		changed := !reflect.DeepEqual(s.activeRuns, current)
		var onChange func()
		if changed {
			s.activeRuns = append([]scheduler.ActiveTaskMeta(nil), current...)
			s.status = observedStatus(current)
			s.rev++
			onChange = s.onChange
		}
		s.mu.Unlock()
		if onChange != nil {
			onChange()
		}
		return false
	}
	s.mu.Lock()
	if !s.observing {
		s.mu.Unlock()
		return true
	}
	s.observing = false
	if s.observeCancel != nil {
		s.observeCancel()
		s.observeCancel = nil
	}
	s.status = nil
	s.attach = nil
	switch {
	case len(current) == 0:
		s.phase = phaseIdle
		s.activeRuns = nil
		s.message = "active metadata 已消失；任务可能已完成或已由另一个实例接管"
	case blocked:
		s.phase = phaseFailed
		s.err = errors.New("原 fastimpl supervisor 已退出或心跳异常，但 active metadata 阶段不安全；请人工检查终端与 worktree")
		s.message = "active worker 不能自动接管"
	default:
		s.phase = phaseIdle
		s.activeRuns = current
		s.message = "原 fastimpl supervisor 已退出；正在准备接管 active worker"
	}
	s.rev++
	onChange := s.onChange
	s.mu.Unlock()
	if onChange != nil {
		onChange()
	}
	return true
}

// Start 启动一次运行。已有运行在进行时返回错误，避免两次调度抢同一批
// issue。opts 里的 RepoDir 覆盖只对这一次运行有效，不写回任何配置。
func (s *session) Start(opts app.Options) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("a run is already in progress")
	}
	if s.observing {
		s.mu.Unlock()
		return errors.New("another live fastimpl instance is being observed read-only; wait for automatic takeover evaluation after it exits")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.cancel = cancel
	s.opts = opts
	if s.writer != nil {
		// 上一次运行的 cleanup 日志在两次运行之间仍可能写入旧文件；开始
		// 新运行时关闭旧句柄，避免每次运行泄漏一个文件描述符。旧 writer
		// 之后的写入会降级为只更新内存快照，不再进入新文件。
		s.writer.closeFile()
	}
	s.writer = &lineWriter{}
	s.phase = phaseStarting
	s.message = "正在加载配置、解析仓库"
	s.prog = graph.BuildProgress{}
	s.err = nil
	s.repoErr = nil
	s.g = nil
	s.sched = nil
	s.attach = nil
	s.spec = specView{}
	s.nodes = nil
	s.ready = nil
	s.status = nil
	s.log = nil
	s.startCh = make(chan struct{})
	s.waitingStart = false
	s.activeRuns = nil
	s.activeScanErr = nil
	s.observing = false
	if s.observeCancel != nil {
		s.observeCancel()
		s.observeCancel = nil
	}
	s.rev++
	onChange := s.onChange
	s.mu.Unlock()
	if onChange != nil {
		onChange()
	}
	go s.run(ctx, opts)
	return nil
}

// Stop 取消运行并等调度器收尾。它不阻塞界面：等待在后台完成，
// 收尾结束后状态才变成已停止。
func (s *session) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// StartScheduling 打开调度门禁。只有依赖图已经建好且管线正等在门禁前
// 时它才生效；重复点击或运行尚未到门禁时直接忽略。
func (s *session) StartScheduling() {
	s.mu.Lock()
	if !s.waitingStart || s.startCh == nil {
		s.mu.Unlock()
		return
	}
	ch := s.startCh
	s.startCh = nil
	s.waitingStart = false
	s.phase = phasePreparing
	s.message = "正在准备集成分支与合并请求"
	s.rev++
	onChange := s.onChange
	s.mu.Unlock()
	close(ch)
	if onChange != nil {
		onChange()
	}
}

// waitForStart 是运行管线里的调度门禁：依赖图已经可见，但准备集成分支、
// 连接 hcgo 与启动 worker 都必须等用户点击开始。显式停止按钮会取消 ctx，
// 门禁随之退出，不会遗留下等待中的 goroutine；普通关闭窗口不取消。
func (s *session) waitForStart(ctx context.Context) error {
	s.mu.Lock()
	s.phase = phaseReady
	s.message = "依赖图已建好；点击开始后才会准备集成并启动 hcgo"
	s.waitingStart = true
	ch := s.startCh
	s.rev++
	onChange := s.onChange
	s.mu.Unlock()
	if onChange != nil {
		onChange()
	}
	select {
	case <-ch:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// failLocal 记录一个还没进入运行管线的本地错误（比如录入的编号
// 不是正整数），界面据此提示而不改变"是否有运行在进行"。
func (s *session) failLocal(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	s.err = err
	s.repoErr = nil
	s.phase = phaseFailed
	s.message = "无法开始"
	s.rev++
}

// run 执行一次 app.RunObserved，把观察者回调写进状态。
func (s *session) run(ctx context.Context, opts app.Options) {
	writer := s.writer
	// 刷新循环跟着这次运行走：正常结束时上游 ctx 不会被取消，没有这
	// 个子 ctx 的话 pump 会一直 tick 下去。
	pumpCtx, stopPump := context.WithCancel(ctx)
	defer stopPump()
	// 从运行一开始就刷新：配置、仓库解析与建图阶段的日志也属于本次
	// 运行，不能等 scheduler ready 后才突然补进界面。
	go s.pump(pumpCtx)
	observer := app.Observer{
		ConfigLoaded: s.configureLogFile,
		Progress: func(p graph.BuildProgress) {
			s.mu.Lock()
			s.phase = phaseBuilding
			s.prog = p
			s.message = progressMessage(p)
			onChange := s.onChange
			s.rev++
			s.mu.Unlock()
			if onChange != nil {
				onChange()
			}
		},
		GraphReady: func(g *graph.Graph) {
			s.mu.Lock()
			s.g = g
			s.phase = phaseReady
			s.message = "依赖图已建好；等待开始调度"
			s.spec = specView{Key: g.Spec(), Title: g.SpecTitle(), Open: g.SpecOpen(), Number: g.Spec().IID, Deps: g.SpecDeps()}
			s.nodes = g.Snapshot()
			s.ready = readyKeys(g)
			onChange := s.onChange
			s.rev++
			s.mu.Unlock()
			if onChange != nil {
				onChange()
			}
		},
		BeforeSchedule: s.waitForStart,
		SchedulerReady: func(sched *scheduler.Scheduler, attach app.AttachTerminal) {
			s.mu.Lock()
			s.sched = sched
			s.attach = attach
			s.phase = phaseScheduling
			s.message = "调度已启动"
			onChange := s.onChange
			s.rev++
			s.mu.Unlock()
			if onChange != nil {
				onChange()
			}
		},
	}
	err := s.runPipeline(ctx, opts, writer, observer)
	var activeConflict *app.ActiveRunConflictError
	if errors.As(err, &activeConflict) {
		// 显式输入 issue 时也可能撞上 live supervisor。app 已在任何写操作前
		// 拒绝；这里把这次本地启动收成 observer，而不是失败页。先清掉本地
		// running/cancel，再由 ObserveActive 建立只读终端快照与 metadata 轮询。
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		s.mu.Unlock()
		s.ObserveActive(activeConflict.Tasks)
		return
	}
	// 结束序列：先在锁内写最终状态，但保持 running 为真——这样别的
	// 启动请求仍会被拒绝，状态不会被新 run 覆盖。refresh 自己要拿
	// s.mu，因此必须在锁外调用。
	s.mu.Lock()
	s.err = err
	s.repoErr = nil
	switch {
	case err == nil:
		s.phase = phaseDone
		s.message = "全部任务已集成"
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		// 取消不是成功：说清楚是被停止的，而不是"完成"。
		s.phase = phaseStopped
		s.message = "已停止；已派发的任务已收尾"
	case app.IsRepositoryError(err):
		var repoErr *app.RepositoryError
		if errors.As(err, &repoErr) {
			s.repoErr = repoErr
		}
		s.phase = phaseFailed
		s.message = "仓库不可用，请在设置里选择仓库目录后重试"
	case ctx.Err() != nil:
		s.phase = phaseStopped
		s.message = "已停止；已派发的任务已收尾"
	default:
		s.phase = phaseFailed
		s.message = "运行失败"
	}
	s.log = writer.lines()
	s.waitingStart = false
	s.startCh = nil
	s.mu.Unlock()

	// 锁外刷新最终快照：refresh 自己会拿 s.mu。
	s.refresh()

	// 最终节点与状态已落定，现在才关掉运行：此后 Start 才可能再开始
	// 一次运行，并且不会覆盖这份最终快照。
	s.mu.Lock()
	s.running = false
	s.cancel = nil
	onChange := s.onChange
	s.mu.Unlock()
	if onChange != nil {
		onChange()
	}
}

// runPipeline 跑这次运行的主体，测试可替换。
func (s *session) runPipeline(ctx context.Context, opts app.Options, writer io.Writer, observer app.Observer) error {
	if s.runFn != nil {
		return s.runFn(ctx, opts, writer, observer)
	}
	return app.RunObserved(ctx, opts, io.Discard, writer, observer)
}

// pump 是本地合并刷新循环：把运行日志、scheduler 快照与图状态同步进界面状态。
// 它只读内存，不发任何网络请求，也不在持锁时做 IO。
func (s *session) pump(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.update()
		}
	}
}

// update 刷新一次并通知界面。
func (s *session) update() {
	s.mu.Lock()
	onChange := s.onChange
	s.mu.Unlock()
	if s.refresh() && onChange != nil {
		onChange()
	}
}

// refresh 把 scheduler 状态与图快照读进来，返回是否有变化。它先取
// 到指针、放锁、再读，避免持着自己的锁去等别的锁。
func (s *session) refresh() bool {
	s.mu.Lock()
	sched, g, writer, observing := s.sched, s.g, s.writer, s.observing
	s.mu.Unlock()
	if observing {
		// observer 的终端快照来自 active metadata，不来自本地 scheduler；
		// 普通 refresh 不能把它覆盖成空。原 supervisor 的生死由 watchObserved
		// 只读轮询负责。
		return false
	}
	var log []string
	if writer != nil {
		log = writer.lines()
	}
	var status []scheduler.TaskRuntime
	if sched != nil {
		status = sched.Status()
	}
	var nodes []graph.Node
	var ready []graph.Key
	var spec specView
	if g != nil {
		nodes = g.Snapshot()
		ready = readyKeys(g)
		spec = specView{Key: g.Spec(), Title: g.SpecTitle(), Open: g.SpecOpen(), Number: g.Spec().IID, Deps: g.SpecDeps()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sameStatus(s.status, status) && sameNodes(s.nodes, nodes) && sameSpec(s.spec, spec) && sameKeys(s.ready, ready) && sameStrings(s.log, log) {
		return false
	}
	s.status, s.nodes, s.spec, s.ready = status, nodes, spec, ready
	s.log = log
	s.rev++
	return true
}

// readyKeys 返回当前可开始工作的节点身份；graph.Ready 已按项目与 IID
// 排序，因此结果可直接比较。
func readyKeys(g *graph.Graph) []graph.Key {
	if g == nil {
		return nil
	}
	nodes := g.Ready()
	keys := make([]graph.Key, len(nodes))
	for i, n := range nodes {
		keys[i] = n.Key
	}
	return keys
}

// sameKeys 比较两份节点身份快照。
func sameKeys(a, b []graph.Key) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameStrings 比较两份字符串快照。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameNodes 比较两份节点快照。
func sameNodes(a, b []graph.Node) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].State != b[i].State || a[i].Closed != b[i].Closed ||
			a[i].InFlight != b[i].InFlight || a[i].Title != b[i].Title || a[i].Kind != b[i].Kind ||
			len(a[i].Deps) != len(b[i].Deps) {
			return false
		}
		for j := range a[i].Deps {
			if a[i].Deps[j] != b[i].Deps[j] {
				return false
			}
		}
	}
	return true
}

// sameStatus 比较两份运行状态快照。
func sameStatus(a, b []scheduler.TaskRuntime) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameSpec 比较两份父 issue 信息。
func sameSpec(a, b specView) bool {
	if a.Key != b.Key || a.Title != b.Title || a.Open != b.Open || a.Number != b.Number || len(a.Deps) != len(b.Deps) {
		return false
	}
	for i := range a.Deps {
		if a.Deps[i] != b.Deps[i] {
			return false
		}
	}
	return true
}

// progressMessage 把建图进度翻成一句人能读的说明。目标数随闭包展开
// 增长，所以只报"已发现/已读取"，不说百分比。
func progressMessage(p graph.BuildProgress) string {
	switch p.Phase {
	case graph.PhaseSpec:
		return "正在读取父 issue"
	case graph.PhaseSeeds:
		return "正在读取子任务与父 issue 的前置"
	case graph.PhaseExpand:
		return fmt.Sprintf("正在展开依赖：已发现 %d 个节点，已读取 %d 个", p.Discovered, p.Read)
	case graph.PhaseCheck:
		return fmt.Sprintf("依赖已收敛（%d 个节点），正在检查环与上限", p.Discovered)
	case graph.PhaseDone:
		return fmt.Sprintf("依赖图建好了：%d 个节点，%d 开放 / %d 已关闭", p.Discovered, p.Open, p.Closed)
	}
	return "正在建依赖图"
}

// lineWriter 收集运行日志：按行切分，只留最近 logLines 行；配置
// log_file 后同时把完整行 append 到文件。文件写入失败只降级为内存日志，
// 绝不让日志后端反过来打断任务运行。
type lineWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	out  []string
	file *os.File
}

// Write 实现 io.Writer。
func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// 没有整行：把读到的部分塞回去等下一次。
			w.buf.Reset()
			w.buf.WriteString(line)
			return len(p), nil
		}
		w.addLocked(strings.TrimRight(line, "\r\n"))
	}
}

// attachFile 把既有日志尾部放到内存历史前面，并启用后续 append。
func (w *lineWriter) attachFile(file *os.File, tail []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(tail) > 0 {
		merged := make([]string, 0, len(tail)+len(w.out))
		merged = append(merged, tail...)
		merged = append(merged, w.out...)
		if len(merged) > logLines {
			merged = append([]string(nil), merged[len(merged)-logLines:]...)
		}
		w.out = merged
	}
	w.file = file
}

// warnLocked 追加一条 GUI 自己的警告；它不写入文件，避免在文件后端
// 失败时递归触发第二次写失败。调用方必须持有 w.mu。
func (w *lineWriter) warnLocked(msg string, err error) {
	line := fmt.Sprintf("time=%s level=WARN msg=%q err=%q", time.Now().Format(time.RFC3339), msg, err.Error())
	w.appendMemoryLocked(line)
}

// appendMemoryLocked 追加一行并裁剪历史。调用方必须持有 w.mu。
func (w *lineWriter) appendMemoryLocked(line string) {
	w.out = append(w.out, line)
	if len(w.out) > logLines {
		w.out = append([]string(nil), w.out[len(w.out)-logLines:]...)
	}
}

// addLocked 追加一行并裁剪历史；文件 append 与内存行保持同样内容。
// 调用方必须持有 w.mu。
func (w *lineWriter) addLocked(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	w.appendMemoryLocked(line)
	if w.file == nil {
		return
	}
	if _, err := fmt.Fprintln(w.file, line); err != nil {
		_ = w.file.Close()
		w.file = nil
		w.warnLocked("log file write failed; continuing with in-memory logs", err)
	}
}

// closeFile 关闭持久日志后端；之后的写仍保留在内存里。
func (w *lineWriter) closeFile() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

// lines 返回已收集的行。
func (w *lineWriter) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.out...)
}

// readLogTail 只读文件尾部 maxBytes 字节，再从其中取最后 maxLines 个
// 非空行。第一行若在窗口中间被截断则丢弃，避免把半行当完整日志展示。
func readLogTail(path string, maxLines int, maxBytes int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 {
		return nil, nil
	}
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if start > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		} else {
			return nil, nil
		}
	}
	rawLines := strings.Split(text, "\n")
	lines := make([]string, 0, min(len(rawLines), maxLines))
	for _, line := range rawLines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > maxLines {
		lines = append([]string(nil), lines[len(lines)-maxLines:]...)
	}
	return lines, nil
}

// configureLogFile 在配置加载后启用持久运行日志。相对路径按本次运行
// 的 WorkDir 解析；文件不存在按空历史处理，读写失败只在日志里降级提示。
func (s *session) configureLogFile(cfg config.Config) {
	name := strings.TrimSpace(cfg.LogFile)
	if name == "" {
		return
	}
	path := filepath.Clean(name)
	if !filepath.IsAbs(path) {
		path = filepath.Join(cfg.WorkDir, path)
	}
	tail, readErr := readLogTail(path, logLines, logFileReadBytes)
	if readErr != nil && !os.IsNotExist(readErr) {
		tail = nil
	}
	file, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	s.mu.Lock()
	writer := s.writer
	s.mu.Unlock()
	if writer == nil {
		if file != nil {
			_ = file.Close()
		}
		return
	}
	if openErr != nil {
		writer.mu.Lock()
		writer.warnLocked("unable to open log file; continuing with in-memory logs", openErr)
		writer.mu.Unlock()
		s.update()
		return
	}
	writer.attachFile(file, tail)
	if readErr != nil && !os.IsNotExist(readErr) {
		writer.mu.Lock()
		writer.warnLocked("unable to read the existing log tail", readErr)
		writer.mu.Unlock()
	}
	s.update()
}
