package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// refreshInterval 是本地快照的合并刷新间隔。它只读内存里的图与
// scheduler 状态，绝不轮询 forge；结果到达时另有一次强制刷新。
const refreshInterval = 400 * time.Millisecond

// logLines 是界面保留的运行日志行数。
const logLines = 200

// phase 是一次运行在界面上的阶段。
type phase int

const (
	// phaseIdle 是还没开始（空状态）。
	phaseIdle phase = iota
	// phaseStarting 是加载配置、解析仓库。
	phaseStarting
	// phaseBuilding 是建依赖图。
	phaseBuilding
	// phasePreparing 是准备集成分支、MR 与 hc 连接。
	phasePreparing
	// phaseScheduling 是调度器在跑。
	phaseScheduling
	// phaseDone 是全部任务已集成。
	phaseDone
	// phaseStopped 是用户停止，调度器已收尾。
	phaseStopped
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
	case phasePreparing:
		return "preparing"
	case phaseScheduling:
		return "scheduling"
	case phaseDone:
		return "done"
	case phaseStopped:
		return "stopped"
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
	status   []scheduler.TaskRuntime
	log      []string
	writer   *lineWriter
	g        *graph.Graph
	sched    *scheduler.Scheduler
	attach   app.AttachTerminal
	cancel   context.CancelFunc
	rev      int
	onChange func()
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
		Revision:    s.rev,
		Phase:       s.phase,
		Issue:       s.opts.SpecIID,
		Progress:    s.prog,
		Message:     s.message,
		Err:         s.err,
		RepoError:   s.repoErr,
		Spec:        s.spec,
		Nodes:       append([]graph.Node(nil), s.nodes...),
		Status:      append([]scheduler.TaskRuntime(nil), s.status...),
		Log:         append([]string(nil), s.log...),
		GraphReady:  s.g != nil,
		Running:     s.running,
		Paths:       s.paths,
		PathsReady:  s.pathsReady,
		PathErr:     s.pathErr,
		RepoPathErr: s.repoPathErr,
		TreePathErr: s.treePathErr,
		AutoStarted: s.autoStarted,
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
		s.mu.Lock()
		s.discovering = false
		s.paths, s.pathErr = preview.Paths, err
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

// Start 启动一次运行。已有运行在进行时返回错误，避免两次调度抢同一批
// issue。opts 里的 RepoDir 覆盖只对这一次运行有效，不写回任何配置。
func (s *session) Start(opts app.Options) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("a run is already in progress")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.cancel = cancel
	s.opts = opts
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
	s.status = nil
	s.log = nil
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
	observer := app.Observer{
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
			s.phase = phasePreparing
			s.message = "依赖图已建好，正在准备集成分支与合并请求"
			s.spec = specView{Key: g.Spec(), Title: g.SpecTitle(), Open: g.SpecOpen(), Number: g.Spec().IID, Deps: g.SpecDeps()}
			s.nodes = g.Snapshot()
			onChange := s.onChange
			s.rev++
			s.mu.Unlock()
			if onChange != nil {
				onChange()
			}
		},
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
			go s.pump(pumpCtx)
		},
	}
	err := s.runPipeline(ctx, opts, writer, observer)
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

// pump 是本地合并刷新循环：把 scheduler 快照与图状态同步进界面状态。
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
	sched, g := s.sched, s.g
	s.mu.Unlock()
	var status []scheduler.TaskRuntime
	if sched != nil {
		status = sched.Status()
	}
	var nodes []graph.Node
	var spec specView
	if g != nil {
		nodes = g.Snapshot()
		spec = specView{Key: g.Spec(), Title: g.SpecTitle(), Open: g.SpecOpen(), Number: g.Spec().IID, Deps: g.SpecDeps()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sameStatus(s.status, status) && sameNodes(s.nodes, nodes) && sameSpec(s.spec, spec) {
		return false
	}
	s.status, s.nodes, s.spec = status, nodes, spec
	s.rev++
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

// lineWriter 收集运行日志：按行切分，只留最近 logLines 行。
type lineWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	out []string
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
		w.add(strings.TrimRight(line, "\r\n"))
	}
}

// add 追加一行并裁剪历史。
func (w *lineWriter) add(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	w.out = append(w.out, line)
	if len(w.out) > logLines {
		w.out = append([]string(nil), w.out[len(w.out)-logLines:]...)
	}
}

// lines 返回已收集的行。
func (w *lineWriter) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.out...)
}
