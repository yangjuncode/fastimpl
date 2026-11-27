// Package scheduler 拥有依赖图与任务状态，把就绪任务分批分配给
// worker：一个任务完成（报告通过判定并串行集成）后，立即按依赖关系
// 释放下一批任务，不等待同一波次的其他任务。
//
// 只调度与 spec 同项目的节点；跨项目节点只作为依赖 gate。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/issuecontext"
	"github.com/yangjuncode/fastimpl/prompt"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// Tick 是调度循环查看图变化的间隔。
const Tick = 500 * time.Millisecond

// heartbeatInterval 是在跑任务无事件时心跳日志的间隔。
const heartbeatInterval = 2 * time.Minute

// RefreshInterval 是周期重读 forge 上全部节点开关状态的间隔：
// 外部关闭的依赖会立即释放后继，无需等任何任务完成。
const RefreshInterval = 10 * time.Minute

// Sessions 是调度器需要的 worker 终端操作；worker.Runner 满足本接口，
// 测试用假实现替换。
type Sessions interface {
	StartWorker(ctx context.Context, p worker.Params) (string, error)
	Snapshot(ctx context.Context, addr string) (worker.Record, error)
	ReadScreen(ctx context.Context, addr string, lines int) (string, bool, error)
	// Stop 结束终端；taskName 是 StartWorker 时使用的完整终端名。
	Stop(ctx context.Context, addr string, specIID int, taskName string) error
	AwaitBoot(ctx context.Context, addr string) (worker.Record, error)
	Prompt(ctx context.Context, addr, text string) (uint64, error)
	WaitTurn(ctx context.Context, addr string, baseline uint64, budget time.Duration) (worker.Record, error)
}

// RuntimeSettings 是一轮运行中可以安全热更新的任务参数快照。
// 它不含 repo/forge/hc/instance/integration 这类已经在构图、连接或
// 恢复身份里消费的字段；这些字段在一轮运行中保持固定。
type RuntimeSettings struct {
	// Parallelism 是后续派发 worker 的并发上限；已经在跑的任务不受影响。
	Parallelism int
	// Retries 是首次尝试之外的续写/冲突重试次数。
	Retries int
	// AgentArgv 是之后新启动 worker/integration agent 的命令行。
	AgentArgv []string
	// RequireReview 控制首次 prompt 与续写 prompt 是否要求双审。
	RequireReview bool
	// Reviewer 是首次 prompt 模板里的第三方 reviewer（已解析默认值）。
	Reviewer string
	// TerminalRetention 是任务收尾时的终端保留策略。
	TerminalRetention string
	// Template 是之后首次 worker prompt 使用的模板文本。
	Template string
}

// RuntimeSettingsLoader 读取一份新的热更新快照。实现方应只读本地
// 文件/内存并快速返回；调度器会在派发、prompt 与重试边界调用它。
type RuntimeSettingsLoader func(context.Context) (RuntimeSettings, error)

// TaskRuntime 是调度器维护的每个任务的运行数据。
type TaskRuntime struct {
	// Key 是任务身份；集成专用 agent 用 spec 的 Key。
	Key graph.Key
	// Addr 是当前 worker 会话的 hc 终端地址；无运行时为空。
	Addr string
	// Label 是终端名（<repo>-issue-<iid>，集成分支的是
	// <repo>-<分支名>），供状态视图给终端页命名，也是人工
	// hc attach 时用的名字。
	Label string
	// Integration 标记这一项是集成分支专属 agent，不是某个任务的 worker。
	Integration bool
	// Attention 是进入 needs attention 的原因，供状态视图显示。
	Attention string
	// ReportReady 表示后台监督已在该终端看到本轮 FASTIMPL-READY 报告；
	// 仍需人工确认后才继续集成。
	ReportReady bool
	// Accepted 是最近一次被接受的提交 SHA。
	Accepted string
	// StartedAt 是本轮 fastimpl 开始使用这个终端的时间；未启动为零值。
	StartedAt time.Time
	// FinishedAt 是 fastimpl 对这个终端的工作结局确定的时间；仍在运行
	// 或尚未启动为零值。终端因保留策略继续存在时，它仍固定在工作结束时刻。
	FinishedAt time.Time
}

// Scheduler 是分批并行调度的核心。
type Scheduler struct {
	cfg       config.Config
	repo      *worktree.Repo
	forge     forge.Forge
	forgeName string // forge 类型（gitlab|github），注入提示词的 {{.Forge}}
	g         *graph.Graph
	sessions  Sessions
	template  string
	workDir   string // fastimpl 运行目录，注入提示词的 {{.WorkDir}}
	intDir    string
	log       *slog.Logger
	specIID   int
	branch    string
	baseName  string             // 集成分支的基点（origin/<baseName>），用于上游同步
	agentArgv []string           // worker 的 agent 命令行（config.AgentArgv 给出）
	warned    map[graph.Key]bool // 已告警过的"closed 但看板未终态"节点，防重复刷日志
	integrate sync.Mutex         // 集成分支上的 merge/push 串行化
	// 以下两个字段只在 integrate 锁内读写（stopIntegrationAgent 在
	// Run 结束后、所有任务 goroutine 退出后读取，无竞争）。
	intAddr      string   // 集成专属 agent 终端地址；空表示未启动
	intAgentArgv []string // 集成专属终端的实际 agent argv，供保留 metadata 精确记录
	baseStuck    bool     // base 冲突 agent 也未能解决，跳过后续同步
	// claimWait 是发 claim 标记后的短等待，claimRecheck 是运行期复查
	// 评论的间隔；测试注入 0/短间隔。
	claimWait    time.Duration
	claimRecheck time.Duration
	// runtime 保存图之外的运行数据（终端地址、注意原因、已接受提交），
	// 由自己的锁保护，供 Status 与 GUI 快照使用。
	runtime runtimeStore
	// retained/retentionMu 跟踪 delayed terminal 的后台清理定时器；
	// retentionChanged 用于 WaitRetention 等待最后一批定时器结束。
	retained         map[terminalID]*retainedTerminal
	retentionMu      sync.Mutex
	retentionChanged chan struct{}
	// runtimeSettingsValue/loader 保存热更新设置。loader 为空时使用
	// New 收到的静态 cfg/template/agentArgv；loader 失败时回退到最后
	// 一份成功加载的快照，避免一次手动写坏文件就打爆已经在跑的任务。
	runtimeSettingsMu     sync.Mutex
	runtimeSettingsValue  RuntimeSettings
	runtimeSettingsLoader RuntimeSettingsLoader
	runtimeSettingsErr    string
	// activeHeartbeat 是测试可缩短的 active metadata heartbeat 间隔。
	activeHeartbeat time.Duration
	// interactiveAttention 为真时，普通失败后的任务不退出 goroutine，
	// 而是 park 在 needs attention 中接受人工刷新/确认/重新调度。CLI 默认
	// 保持旧行为：Run 在无就绪任务时返回 stalled error。
	interactiveAttention atomic.Bool
	// attentionPoll 是 parked worker 的监督间隔；测试可缩短。
	attentionPoll time.Duration
	// attentionControls 保存 parked 任务的控制入口。只有任务 goroutine
	// 自己消费，ControlNode 按 key 序列化转发。
	attentionMu       sync.Mutex
	attentionControls map[graph.Key]*attentionControl
}

// New 组装调度器；所有依赖由 app 层注入，便于测试替换。
// intDir/intBranch/baseName 均来自 EnsureIntegration 的实际返回，
// 复用带描述后缀或配置覆盖的分支名时也能正确合并与推送。
func New(cfg config.Config, repo *worktree.Repo, f forge.Forge, forgeName string, g *graph.Graph, sessions Sessions, template, workDir, intDir, intBranch, baseName string, agentArgv []string, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		cfg:                  cfg,
		repo:                 repo,
		forge:                f,
		forgeName:            forgeName,
		g:                    g,
		sessions:             sessions,
		template:             template,
		workDir:              workDir,
		intDir:               intDir,
		log:                  log,
		specIID:              g.Spec().IID,
		branch:               intBranch,
		baseName:             baseName,
		agentArgv:            agentArgv,
		warned:               make(map[graph.Key]bool),
		claimWait:            DefaultClaimWait,
		claimRecheck:         DefaultClaimRecheck,
		retained:             make(map[terminalID]*retainedTerminal),
		retentionChanged:     make(chan struct{}),
		runtimeSettingsValue: runtimeSettingsFromConfig(cfg, template, agentArgv),
		activeHeartbeat:      ActiveHeartbeatInterval,
		attentionPoll:        5 * time.Second,
		attentionControls:    make(map[graph.Key]*attentionControl),
	}
}

func runtimeSettingsFromConfig(cfg config.Config, template string, agentArgv []string) RuntimeSettings {
	return RuntimeSettings{
		Parallelism:       cfg.Parallelism,
		Retries:           cfg.Retries,
		AgentArgv:         append([]string(nil), agentArgv...),
		RequireReview:     cfg.RequireReview,
		Reviewer:          cfg.ReviewerName(),
		TerminalRetention: cfg.TerminalRetention,
		Template:          template,
	}
}

func copyRuntimeSettings(in RuntimeSettings) RuntimeSettings {
	out := in
	out.AgentArgv = append([]string(nil), in.AgentArgv...)
	return out
}

func validateRuntimeSettings(settings RuntimeSettings) error {
	if settings.Parallelism < 1 {
		return fmt.Errorf("parallelism must be at least 1, got %d", settings.Parallelism)
	}
	if settings.Retries < 0 {
		return fmt.Errorf("retries must not be negative, got %d", settings.Retries)
	}
	if len(settings.AgentArgv) == 0 {
		return fmt.Errorf("agent command is empty")
	}
	if _, _, err := config.ParseTerminalRetention(settings.TerminalRetention); err != nil {
		return err
	}
	return nil
}

// SetRuntimeSettingsLoader 安装热更新读取器。app 层用它把 GUI 保存到
// 工作目录的配置/提示词带进已经开始的调度；测试不传 loader 时保持
// New 收到的静态行为。
func (s *Scheduler) SetRuntimeSettingsLoader(loader RuntimeSettingsLoader) {
	s.runtimeSettingsMu.Lock()
	s.runtimeSettingsLoader = loader
	s.runtimeSettingsMu.Unlock()
}

// currentRuntimeSettings 返回一份可安全使用的热更新快照。读取失败时
// 回退到最后一份成功快照并记录 warn：已经在跑的任务不能因为一次
// 手动编辑中的非法文件而全部失败。
func (s *Scheduler) currentRuntimeSettings(ctx context.Context) RuntimeSettings {
	s.runtimeSettingsMu.Lock()
	defer s.runtimeSettingsMu.Unlock()
	if s.runtimeSettingsLoader == nil {
		return copyRuntimeSettings(runtimeSettingsFromConfig(s.cfg, s.template, s.agentArgv))
	}
	settings, err := s.runtimeSettingsLoader(ctx)
	if err == nil {
		err = validateRuntimeSettings(settings)
	}
	if err != nil {
		msg := err.Error()
		if s.runtimeSettingsErr != msg {
			s.log.Warn("reload runtime settings failed; using the last good snapshot", "err", err)
		}
		s.runtimeSettingsErr = msg
		return copyRuntimeSettings(s.runtimeSettingsValue)
	}
	settings.AgentArgv = append([]string(nil), settings.AgentArgv...)
	s.runtimeSettingsValue = copyRuntimeSettings(settings)
	s.runtimeSettingsErr = ""
	return copyRuntimeSettings(settings)
}

// issuePromptContext 读取 graph build 阶段物化的本地 issue 快照，并包装成
// prompt 段。快照只是启动加速：文件不存在/不可读时返回空，模板仍走
// 原来的 forge 读取兜底，不能把任务启动变成失败。
func (s *Scheduler) issuePromptContext(key graph.Key) (section, path string, truncated bool) {
	if s.repo == nil || s.repo.Name == "" {
		return "", "", false
	}
	path = issuecontext.FilePath(issuecontext.Dir(s.repo.Name), key)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	markdown := strings.TrimSpace(string(data))
	if markdown == "" {
		return "", "", false
	}
	return issuecontext.PromptSection(markdown, path), path, len(markdown) > issuecontext.PromptContextLimit
}

// forgeCLI 返回 forge 类型对应的 issue 命令行工具名；
// 未识别的类型返回空（提示词相应段自行退化）。
func forgeCLI(name string) string {
	switch name {
	case config.ForgeGitLab:
		return "glab"
	case config.ForgeGitHub:
		return "gh"
	}
	return ""
}

// taskResult 是一个任务 goroutine 的结局。
type taskResult struct {
	key        graph.Key
	integrated bool
	closed     bool // 开跑前发现 issue 已被外部关闭：不当失败，只标记
	restart    bool // 人工清理后重新排队：清理照旧做，图状态回到 Pending
	commit     string
	attention  string
}

// Run 是调度主循环：就绪即派、完成即放，直到全部同项目工作项
// Closed/Integrated/Failed；无就绪且无在跑但图未完成时报停滞错误。
func (s *Scheduler) Run(ctx context.Context) error {
	s.reconcileRetained(ctx)
	defer s.stopIntegrationAgent()
	running := make(map[graph.Key]bool)
	results := make(chan taskResult, s.cfg.Parallelism)
	refresh := time.NewTicker(RefreshInterval)
	defer refresh.Stop()
	lastBeat := time.Now()
	for {
		if ctx.Err() != nil {
			// 上游取消：不再派发，等已派任务把清理（停终端、释放认领与
			// 本地锁）做完再返回，避免提前关 hc 连接导致清理竞争。
			return s.drain(ctx, running, results)
		}
		ready := s.g.Ready()
		runtimeSettings := s.currentRuntimeSettings(ctx)
		for _, n := range ready {
			if len(running) >= runtimeSettings.Parallelism {
				break
			}
			if running[n.Key] {
				continue
			}
			running[n.Key] = true
			s.g.SetState(n.Key, graph.Running)
			s.log.Info("task dispatched", "key", n.Key, "title", n.Title)
			go s.runTask(ctx, n.Key, results)
		}
		if len(running) == 0 {
			if s.g.Done() {
				s.log.Info("all tasks integrated", "branch", s.branch)
				return nil
			}
			if len(ready) == 0 {
				return s.stalledError()
			}
		}
		// 心跳：任务在跑但长时间无事件时定期报一声在做什么——
		// agent 一轮最长 TurnBudget，期间主循环完全静默。
		if len(running) > 0 && time.Since(lastBeat) >= heartbeatInterval {
			lastBeat = time.Now()
			keys := make([]graph.Key, 0, len(running))
			for k := range running {
				keys = append(keys, k)
			}
			slices.SortFunc(keys, func(a, b graph.Key) int { return a.IID - b.IID })
			s.log.Info("in progress", "running", len(running), "keys", keys)
		}
		select {
		case res := <-results:
			lastBeat = time.Now()
			delete(running, res.key)
			s.record(res)
		case <-refresh.C:
			s.refreshStates(ctx)
		case <-time.After(Tick):
		}
	}
}

// drain 停止派发并等已派任务把清理（停终端、释放认领与本地锁）做完：
// 结果照常更新图，但不再派新任务。返回 ctx 的原始错误。
func (s *Scheduler) drain(ctx context.Context, running map[graph.Key]bool, results <-chan taskResult) error {
	err := ctx.Err()
	s.log.Warn("run cancelled; waiting for dispatched tasks to clean up", "running", len(running), "reason", err)
	for len(running) > 0 {
		res := <-results
		delete(running, res.key)
		s.record(res)
	}
	return err
}

// record 把一次结局写进图与运行数据并记录日志。
func (s *Scheduler) record(res taskResult) {
	// 结局到达主循环时，本轮工作任务已经结束；只有真正启动过终端的
	// 任务才会因此得到 FinishedAt（预检失败的条目保持零值）。
	s.runtime.finish(res.key, time.Now())
	switch {
	case res.integrated:
		s.g.SetState(res.key, graph.Integrated)
		// 集成成功路径已经调用 forge.Close 关闭了远端 issue，
		// 立刻同步内存图的 Closed 状态，避免依赖 10 分钟后的 refreshStates
		// 才翻成已关闭，导致最后一个任务停留在未关闭状态。
		s.g.SetClosed(res.key, true)
		s.runtime.setAccepted(res.key, res.commit)
		s.runtime.setAttention(res.key, "")
		s.runtime.setReportReady(res.key, false)
		s.log.Info("task integrated", "key", res.key, "commit", res.commit)
	case res.closed:
		s.g.SetClosed(res.key, true)
		s.runtime.clearAddr(res.key)
		s.runtime.setAttention(res.key, "")
		s.runtime.setReportReady(res.key, false)
		s.log.Info("issue already closed; skipped", "key", res.key)
	case res.restart:
		s.g.SetState(res.key, graph.Pending)
		s.runtime.clearAddr(res.key)
		s.runtime.setAttention(res.key, "")
		s.runtime.setReportReady(res.key, false)
		s.log.Info("task returned to pending for rescheduling", "key", res.key)
	default:
		s.g.SetState(res.key, graph.NeedsAttention)
		s.runtime.setAttention(res.key, res.attention)
		s.log.Warn("task needs attention", "key", res.key, "reason", res.attention)
	}
}

// refreshStates 周期重读所有节点在 forge 上的开关与占用状态：
// 外部关闭的依赖释放后继，重新打开的恢复 gating；看板被他人接管
// 的节点标记 InFlight 不就绪，退回空闲后自动恢复。
// 单个节点读取失败只告警不中断本次刷新。
func (s *Scheduler) refreshStates(ctx context.Context) {
	for _, n := range s.g.Snapshot() {
		info, err := s.forge.IssueState(ctx, n.Key.ProjectID, n.Key.IID)
		if err != nil {
			s.log.Warn("refresh issue state failed", "key", n.Key, "err", err)
			continue
		}
		if n.Closed == info.Open {
			s.g.SetClosed(n.Key, !info.Open)
			s.log.Info("issue state refreshed", "key", n.Key, "closed", !info.Open)
		}
		if n.InFlight != info.InFlight {
			s.g.SetInFlight(n.Key, info.InFlight)
			s.log.Info("issue occupancy refreshed", "key", n.Key, "in_flight", info.InFlight)
		}
		// 已关闭但看板 status 未进终态：只告警提醒人工修正，
		// 不自动改——不动非本运行执行的 issue 的看板。每节点每轮
		// 只 warn 一次，防止周期刷新刷日志。
		if !info.Open && !info.DoneOnBoard && !s.warned[n.Key] {
			s.warned[n.Key] = true
			s.log.Warn("issue closed but board status not in a done column; fix manually",
				"key", n.Key)
		}
	}
}

// stalledError 报告调度停滞：无可就绪、无在跑、图未完成。
// 列出 needs attention 的节点与被开放跨项目前置卡住的节点。
func (s *Scheduler) stalledError() error {
	var attention, blocked, occupied []string
	for _, n := range s.g.Snapshot() {
		if n.Closed {
			continue
		}
		switch n.State {
		case graph.NeedsAttention:
			attention = append(attention, n.Key.String())
		case graph.Pending:
			if n.InFlight {
				occupied = append(occupied, n.Key.String())
			} else if n.Key.ProjectID == s.g.Spec().ProjectID && s.hasOpenForeignDep(n) {
				blocked = append(blocked, n.Key.String())
			}
		}
	}
	msg := "scheduler stalled"
	if len(attention) > 0 {
		msg += fmt.Sprintf("; needs attention: %v", attention)
	}
	if len(occupied) > 0 {
		msg += fmt.Sprintf("; occupied (in progress elsewhere): %v", occupied)
	}
	if len(blocked) > 0 {
		msg += fmt.Sprintf("; blocked by open cross-project deps: %v", blocked)
	}
	return fmt.Errorf("%s", msg)
}

// hasOpenForeignDep 报告节点是否有开放的跨项目前置（只能等外部关闭）。
func (s *Scheduler) hasOpenForeignDep(n graph.Node) bool {
	for _, dep := range n.Deps {
		if dep.ProjectID == s.g.Spec().ProjectID {
			continue
		}
		if depNode, ok := s.g.Lookup(dep); ok && !depNode.Closed {
			return true
		}
	}
	return false
}

// intTermName 是集成分支专属 agent 的终端名：<repo>-<集成分支名>。
func (s *Scheduler) intTermName() string {
	return s.repo.Name + "-" + s.branch
}

// baseConflictPrompt 发给集成 worktree 专属 agent 的提示词：
// 让它自己执行 git merge origin/<base> 解决上游冲突并提交；
// 明确禁止丢弃集成分支上已有的任务成果。
func (s *Scheduler) baseConflictPrompt(base string, files []string, token string) string {
	return fmt.Sprintf("你的工作目录 %s 是 fastimpl 创建的集成分支 worktree（agent 启动目录即此目录）。请执行 git merge origin/%s 把上游合入集成分支，逐个解决冲突（当前冲突文件：%s）并提交合并。集成分支上已有其他任务的成果，禁止 reset、rebase 或 checkout 丢弃任何已有提交。冲突解决并完成合并提交后，在回复末尾单独一行输出 %s %s；未完成前不要输出该标记。",
		s.intDir, base, strings.Join(files, ", "), worker.ReadyMarker, token)
}

// baseConflictContinuePrompt 是 base 冲突 agent 未出报告时的续写提示词。
// 不套用实现 worker 的 worker.ContinuePrompt：实现/验证/审核的措辞与
// 纯合并任务无关，这里只重申完成合并提交后输出报告标记的契约；不再列
// 冲突文件——续写时清单可能已变化，agent 以 git status 为准。
func baseConflictContinuePrompt(base, token string) string {
	return fmt.Sprintf("继续完成合并：完成 git merge origin/%s 的冲突解决并提交合并（冲突文件以 git status 为准；禁止 reset、rebase 或 checkout 丢弃集成分支上已有的任何提交）。完成合并提交后，在回复末尾单独一行输出 %s %s；未完成前不要输出该标记。",
		base, worker.ReadyMarker, token)
}

// conflictPrompt 是让 agent 自己解决集成冲突的续写提示词：
// 在任务 worktree（dir，不是 agent 的 cwd）中把集成分支合入本分支、
// 解决冲突并提交，然后输出报告标记；fastimpl 之后再做干净合并。
func conflictPrompt(dir, intBranch string, files []string, token string) string {
	return fmt.Sprintf("你的分支与集成分支 %s 存在冲突（%s）。请在本任务 worktree %s 中执行 git merge %s（可用 git -C %s），逐个解决冲突并提交。冲突解决并提交完成后，在回复末尾单独一行输出 %s %s，随后最多六行说明解决方式；未解决前不要输出该标记。",
		intBranch, strings.Join(files, ", "), dir, intBranch, dir, worker.ReadyMarker, token)
}

// captureReport 读取终端屏幕并解析本任务令牌的 READY 帧。
func (s *Scheduler) captureReport(ctx context.Context, addr, token string) (worker.Report, error) {
	screen, complete, err := s.sessions.ReadScreen(ctx, addr, worker.CaptureLines)
	if err != nil {
		return worker.Report{}, fmt.Errorf("read worker screen: %w", err)
	}
	if !complete {
		return worker.Report{}, fmt.Errorf("worker screen %s is not fully readable", addr)
	}
	return worker.ParseReport(screen, token)
}

// mergeProgress 是任务 active metadata 的集成阶段回调；nil 表示调用方
// 不持久化进度（目前只有测试或直接内部调用会这样）。写失败必须显式
// 返回：后续副作用是否安全，取决于崩溃恢复能否看见正确的阶段。
type mergeProgress func(func(*ActiveTaskMeta)) error

func updateMergeProgress(progress mergeProgress, fn func(*ActiveTaskMeta)) error {
	if progress != nil {
		return progress(fn)
	}
	return nil
}

// tryMerge 在串行锁内先把上游 base 同步进集成分支，
// 再合任务分支并推送。base 冲突交给集成分支专属 agent 解决；
// 解决不了则标记 baseStuck 跳过后续同步，任务合并照常进行，
// 冲突留给 MR/人工处理。
//
// 每个副作用步骤前先更新 active metadata 的 IntegrationPhase：崩溃重启
// 后普通 sync/merge/push 可以幂等重入，base 冲突 agent 则凭保存的
// token/baseline 继续等待，而不是重启第二个 agent。
func (s *Scheduler) tryMerge(ctx context.Context, branch string, progress mergeProgress, recovery *ActiveTaskMeta) error {
	s.integrate.Lock()
	defer s.integrate.Unlock()
	if !s.baseStuck {
		if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) { m.IntegrationPhase = integrationPhaseSyncingBase }); err != nil {
			return fmt.Errorf("record base-sync integration phase: %w", err)
		}
		err := s.repo.SyncBase(ctx, s.intDir, s.baseName)
		var conflict *worktree.ConflictError
		switch {
		case err == nil:
		case errors.As(err, &conflict):
			if rerr := s.resolveBaseConflict(ctx, conflict.Files, progress, recovery); rerr != nil {
				s.baseStuck = true
				s.log.Warn("base conflict unresolved; skipping further sync",
					"base", s.baseName, "err", rerr, "inspect", "hc attach "+s.intTermName())
			}
		default:
			s.log.Warn("base sync failed; continuing", "base", s.baseName, "err", err)
		}
	}
	if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) { m.IntegrationPhase = integrationPhaseMergingTask }); err != nil {
		return fmt.Errorf("record task-merge integration phase: %w", err)
	}
	if err := s.repo.Merge(ctx, s.intDir, branch); err != nil {
		return err
	}
	if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) { m.IntegrationPhase = integrationPhasePushing }); err != nil {
		return fmt.Errorf("record push integration phase: %w", err)
	}
	if err := s.repo.Push(ctx, s.intDir, s.branch); err != nil {
		return err
	}
	if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) {
		m.IntegrationPhase = ""
		m.IntegrationTerminalAddr = ""
		m.IntegrationTerminalName = ""
		m.IntegrationTerminalCwd = ""
		m.IntegrationTerminalPID = 0
		m.IntegrationTerminalArgv = nil
		m.IntegrationToken = ""
		m.IntegrationBaseline = 0
		m.IntegrationAttempts = 0
		m.IntegrationPromptSent = false
	}); err != nil {
		return fmt.Errorf("clear integration recovery state: %w", err)
	}
	return nil
}

// resolveBaseConflict 让集成分支专属 agent 在集成 worktree 中解决
// origin/<base> 与集成分支的冲突：懒启动/接管终端 → 发冲突提示词 →
// 等标记 → 重跑 SyncBase 验证干净。recovery 带有完整 token/baseline 时，
// 直接继续等待原终端，绝不重发 prompt 或启动第二个集成 agent。
// 调用方必须持有 integrate 锁——整个解决过程独占集成 worktree。
func (s *Scheduler) resolveBaseConflict(ctx context.Context, files []string, progress mergeProgress, recovery *ActiveTaskMeta) error {
	termName := s.intTermName()
	recoveringPrompt := false
	if recovery != nil && recovery.IntegrationPhase == integrationPhaseResolvingBaseConflict && recovery.IntegrationPromptSent {
		recoveringPrompt = true
	}
	token := worker.NewToken()
	baseline := uint64(0)
	attempts := 0
	needPrompt := true
	if recoveringPrompt {
		token = recovery.IntegrationToken
		baseline = recovery.IntegrationBaseline
		attempts = recovery.IntegrationAttempts
		needPrompt = false
	}
	if !recoveringPrompt {
		// 在启动/BOOT 之前先落一个不完整恢复点。若崩溃发生在终端已经启动、
		// 但完整进程身份尚未写入之间，恢复方会因字段不完整而保守阻塞，绝不会
		// 误以为“还没有集成 agent”而再启动一个。正常流程随后会用完整身份覆盖它。
		if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) {
			m.IntegrationPhase = integrationPhaseResolvingBaseConflict
			m.IntegrationTerminalAddr = ""
			m.IntegrationTerminalName = ""
			m.IntegrationTerminalCwd = ""
			m.IntegrationTerminalPID = 0
			m.IntegrationTerminalArgv = nil
			m.IntegrationToken = token
			m.IntegrationBaseline = baseline
			m.IntegrationAttempts = attempts
			m.IntegrationPromptSent = false
		}); err != nil {
			return fmt.Errorf("record base-conflict startup state: %w", err)
		}
	}
	if s.intAddr == "" {
		var addr string
		if recoveringPrompt {
			// recoveryCandidate 已核验 addr/name/labels/live。这里只接管，
			// 不 StartWorker、不 Prompt。
			addr = recovery.IntegrationTerminalAddr
			termName = recovery.IntegrationTerminalName
			s.runtime.setAddr(s.g.Spec(), addr, termName, true)
			s.runtime.start(s.g.Spec(), time.Now())
			s.intAddr = addr
			s.log.Info("integration worker recovered", "terminal", termName, "addr", addr)
		} else {
			var reused bool
			var err error
			integrationSettings := s.currentRuntimeSettings(ctx)
			addr, reused, err = s.claimRetainedTerminal(ctx, s.g.Spec(), s.specIID, true, termName, s.intDir, integrationSettings.AgentArgv)
			if err != nil {
				return err
			}
			if !reused {
				addr, err = s.sessions.StartWorker(ctx, worker.Params{
					SpecIID:  s.specIID,
					IssueIID: s.specIID,
					Name:     termName,
					Worktree: s.intDir,
					Argv:     integrationSettings.AgentArgv,
				})
				if err != nil {
					return fmt.Errorf("start integration worker: %w", err)
				}
			}
			// 集成 agent 用 spec 的 key 与独立标签：任务的 key 是子任务，
			// 不会与它冲突。地址在 boot 之前就公开：启动阶段最可能卡在
			// 权限询问上，正是要人看的时候。
			s.runtime.setAddr(s.g.Spec(), addr, termName, true)
			s.runtime.start(s.g.Spec(), time.Now())
			if _, err := s.sessions.AwaitBoot(ctx, addr); err != nil {
				return fmt.Errorf("await integration worker boot: %w", err)
			}
			s.intAddr = addr
			s.log.Info("integration worker running", "terminal", termName, "addr", addr)
		}
	} else {
		termName = s.intTermName()
	}

	// 集成终端与普通 worker 一样必须携带进程身份。recovery 使用原 metadata；
	// 新启动/复用/同轮再接管的终端则从 hc snapshot 现读。读不到时不发 prompt，
	// 避免制造一个崩溃后无法安全辨认的集成 agent。
	intCwd, intPID := "", 0
	var intArgv []string
	if recoveringPrompt {
		intCwd = recovery.IntegrationTerminalCwd
		intPID = recovery.IntegrationTerminalPID
		intArgv = append([]string(nil), recovery.IntegrationTerminalArgv...)
	} else {
		record, err := s.sessions.Snapshot(ctx, s.intAddr)
		if err != nil {
			return fmt.Errorf("read integration worker process identity: %w", err)
		}
		intCwd = record.Cwd
		intPID = record.PID
		intArgv = append([]string(nil), record.Argv...)
	}
	if intCwd == "" || intPID <= 0 || len(intArgv) == 0 {
		return fmt.Errorf("integration worker %s does not expose verifiable cwd/pid/argv", termName)
	}
	s.intAgentArgv = append([]string(nil), intArgv...)
	// 先落 terminal/token/phase，再发送 prompt；prompt 成功后再落
	// prompt_sent+baseline。中间崩溃会让恢复方保守阻塞，而不是猜。
	if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) {
		m.IntegrationPhase = integrationPhaseResolvingBaseConflict
		m.IntegrationTerminalAddr = s.intAddr
		m.IntegrationTerminalName = termName
		m.IntegrationTerminalCwd = intCwd
		m.IntegrationTerminalPID = intPID
		m.IntegrationTerminalArgv = append([]string(nil), intArgv...)
		m.IntegrationToken = token
		m.IntegrationBaseline = baseline
		m.IntegrationAttempts = attempts
		m.IntegrationPromptSent = !needPrompt
	}); err != nil {
		return fmt.Errorf("record base-conflict integration state: %w", err)
	}
	if needPrompt {
		var err error
		baseline, err = s.sessions.Prompt(ctx, s.intAddr, s.baseConflictPrompt(s.baseName, files, token))
		if err != nil {
			return fmt.Errorf("send base-conflict prompt: %w", err)
		}
		if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) {
			m.IntegrationBaseline = baseline
			m.IntegrationAttempts = attempts
			m.IntegrationPromptSent = true
		}); err != nil {
			return fmt.Errorf("record base-conflict prompt state: %w", err)
		}
	} else {
		s.log.Info("recovered integration prompt already in flight; waiting", "terminal", termName, "baseline", baseline)
	}
	for {
		if _, err := s.sessions.WaitTurn(ctx, s.intAddr, baseline, worker.IntegrationTurnBudget); err != nil {
			return fmt.Errorf("wait integration worker turn: %w", err)
		}
		if _, err := s.captureReport(ctx, s.intAddr, token); err == nil {
			// 标记出现：重跑 SyncBase 应干净（merge 已提交或 up-to-date）；
			// agent 未完成/没提交则重跑仍会报错，进入下一轮续写。
			if err := s.repo.SyncBase(ctx, s.intDir, s.baseName); err == nil {
				return nil
			}
		}
		if attempts >= s.currentRuntimeSettings(ctx).Retries {
			return fmt.Errorf("base merge conflict unresolved after %d retries", attempts)
		}
		attempts++
		var err error
		baseline, err = s.sessions.Prompt(ctx, s.intAddr, baseConflictContinuePrompt(s.baseName, token))
		if err != nil {
			return fmt.Errorf("send continue prompt: %w", err)
		}
		if err := updateMergeProgress(progress, func(m *ActiveTaskMeta) {
			m.IntegrationBaseline = baseline
			m.IntegrationAttempts = attempts
			m.IntegrationPromptSent = true
		}); err != nil {
			return fmt.Errorf("record base-conflict continue state: %w", err)
		}
	}
}

// stopIntegrationAgent 在 Run 结束时回收集成专属终端；
// base 冲突未解决时保留终端供 hc attach 排查——此时地址也留着，
// GUI 仍能连上去看。
func (s *Scheduler) stopIntegrationAgent() {
	if s.intAddr == "" {
		return
	}
	// 不管终端最后是停止、保留还是留给人工排查，fastimpl 本轮使用它的
	// 工作都已在 Run 收尾时结束。
	s.runtime.finish(s.g.Spec(), time.Now())
	if s.baseStuck {
		s.log.Warn("integration worker left running for inspection",
			"inspect", "hc attach "+s.intTermName())
		return
	}
	delay, never := s.retentionPolicy()
	if delay != 0 || never {
		agentArgv := append([]string(nil), s.intAgentArgv...)
		if len(agentArgv) == 0 {
			agentArgv = append([]string(nil), s.agentArgv...)
		}
		s.retainTerminal(s.g.Spec(), s.specIID, true, s.intAddr, s.intTermName(), s.intDir, s.branch, "", agentArgv)
		return
	}
	if err := s.sessions.Stop(context.Background(), s.intAddr, s.specIID, s.intTermName()); err != nil {
		s.log.Warn("stop integration worker failed", "addr", s.intAddr, "err", err)
		return
	}
	s.runtime.clearAddr(s.g.Spec())
}

// 任务收尾策略：是否需要停终端、是否保留两把锁。
const (
	// finishNone：还没有可能存活的终端，直接释放两把锁。
	finishNone = iota
	// finishKeep：无法确认终端已停止（启动结果不明/普通运行失败），
	// 保留终端、本地锁与远端认领，等人工排查。
	finishKeep
	// finishStop：明确失锁或上游取消，尝试停终端，成功才释放。
	finishStop
)

// runTask 执行一个任务的完整生命周期，结局经 results 回报。
// 完成判定 = 屏幕上出现本任务令牌的 FASTIMPL-READY 行；
// 没有标记时按 cfg.Retries 向同一终端续写提示词重试；
// 合并冲突时让 agent 在任务 worktree 里解决后重试快速合并。
//
// 跨机器互斥靠两条链：本机 O_EXCL 本地锁，以及 issue 评论里的认领标记
// （尽力仲裁，评论 API 没有原子互斥，短暂双运行要到复查才发现）。
func (s *Scheduler) runTask(ctx context.Context, key graph.Key, results chan<- taskResult) {
	var res taskResult
	res.key = key
	var note string
	defer func() {
		if note != "" {
			nctx, cancel := s.cleanupCtx(ctx)
			if nerr := s.forge.Note(nctx, key.ProjectID, key.IID, note); nerr != nil {
				s.log.Warn("issue note failed", "key", key, "err", nerr)
			}
			cancel()
		}
		// 清理先于结果发布：调度器看到结局时资源已回收。
		results <- res
	}()
	// fail 记录一次普通运行失败：保留终端排查，稍后写一条 issue 报告。
	fail := func(err error) {
		res.attention = err.Error()
		note = "fastimpl: " + err.Error()
	}
	iid := key.IID

	if !config.ValidInstanceID(s.cfg.InstanceID) {
		res.attention = "instance_id is missing or unsafe; refusing to run without cross-machine identity"
		s.runtime.setAttention(key, res.attention)
		s.log.Error("task refused", "key", key, "reason", res.attention)
		return
	}
	startSettings := s.currentRuntimeSettings(ctx)

	// 先看有没有崩溃/关闭后留下的 active worker。只要 metadata 表明
	// 另一个 supervisor 仍可能活着，或者接管边界不安全，就绝不启动新 agent。
	recovery, err := s.recoveryCandidate(ctx, key)
	if err != nil {
		res.attention = err.Error()
		s.runtime.setAttention(key, res.attention)
		s.log.Warn("active worker recovery refused", "key", key, "err", err)
		return
	}
	recovered := recovery != nil

	token := worker.NewToken()
	reportToken := worker.NewToken()
	if recovered {
		// claim/report token 都是原任务协议的一部分：claim 用来继续证明
		// 跨机器所有权，report token 用来识别原 agent 已经写出的完成标记。
		token = recovery.ClaimToken
		reportToken = recovery.ReportToken
	}
	lock, releaseAdoption, err := acquireTaskLockAfterRecovery(s.repo.Root, iid, lockContent(os.Getpid(), s.cfg.InstanceID, token), recovered, recovery)
	if err != nil {
		res.attention = err.Error()
		s.runtime.setAttention(key, res.attention)
		s.log.Warn("local task lock refused", "key", key, "err", err,
			"hint", "confirm no worker for this issue is running, then delete the lock file")
		return
	}

	// 收尾状态：markerPosted 表示 issue 上已有本次任务协议的 claim 标记；
	// addr/termName 非空表示可能还有存活的终端。recovery 的 marker 是原
	// supervisor 发的，但它使用同一个 token；接管后由我们负责释放。
	markerPosted := recovered
	addr, termName := "", ""
	finish := finishNone
	if recovered && recovery.TerminalAddr != "" {
		finish = finishKeep
	}
	stopped := false

	var dir, branch string
	var stopAndRelease func(branch, commit string) error

	now := time.Now()
	meta := ActiveTaskMeta{
		Version:           activeMetaVersion,
		RepoDir:           CanonicalRepoPath(s.repo.Root),
		WorkDir:           s.workDir,
		TreeDir:           s.repo.TreeDir,
		IntegrationDir:    s.intDir,
		ProjectID:         key.ProjectID,
		SpecIID:           s.specIID,
		IssueIID:          iid,
		IntegrationBranch: s.branch,
		BaseBranch:        s.baseName,
		HCBinary:          s.cfg.HcBinary,
		HCSocket:          s.cfg.HcSocket,
		ClaimToken:        token,
		ReportToken:       reportToken,
		Stage:             activeStageClaiming,
	}
	if recovered {
		meta = *recovery
		meta.HCBinary = s.cfg.HcBinary
		meta.HCSocket = s.cfg.HcSocket
		meta.ClaimToken = token
		meta.ReportToken = reportToken
	}
	meta.Supervisor = currentSupervisor(s.cfg.InstanceID)
	meta.UpdatedAt = now
	activePath := ActiveMetaPath(s.repo.Root, iid)
	activeW, activeErr := newActiveWriter(activePath, meta)
	if activeErr != nil {
		// active metadata 是接管协议的必要条件，不是可选调试信息。此时还
		// 没有发新 claim、也没有启动/接管控制 worker：释放刚拿到的本地锁并
		// fail-closed。recovery 场景下原 claim/metadata 保留，下一次还可重试。
		if relErr := lock.Release(); relErr != nil {
			s.log.Warn("release task lock after active metadata failure", "key", key, "lock", lock.Path(), "err", relErr)
		}
		if err := releaseAdoption(); err != nil {
			s.log.Warn("release active recovery adoption lock failed", "key", key, "err", err)
		}
		res.attention = fmt.Sprintf("write active worker metadata: %v", activeErr)
		s.runtime.setAttention(key, res.attention)
		s.log.Warn("task refused because active metadata is not durable", "key", key, "path", activePath, "err", activeErr)
		return
	}
	if recovered {
		// metadata 已经指向本进程后才释放 adoption mutex；此后另一个实例
		// 会看到 live supervisor 而只读观察，不会删除我们刚创建的锁。
		if err := releaseAdoption(); err != nil {
			s.log.Warn("release active recovery adoption lock failed", "key", key, "err", err)
		}
	}
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	if activeW != nil {
		s.startActiveHeartbeat(heartbeatCtx, activeW)
	}
	updateActive := func(fn func(*ActiveTaskMeta)) error {
		if activeW == nil {
			return nil
		}
		if err := activeW.update(fn); err != nil {
			s.log.Warn("update active worker metadata failed", "key", key, "path", activePath, "err", err)
			return err
		}
		return nil
	}
	defer func() {
		stopHeartbeat()
		if finish == finishKeep && res.attention != "" && s.interactiveAttention.Load() && addr != "" {
			// 结果要等人工处理完才发布；先把图节点标成 needs attention，
			// 让 GUI/监督循环在 park 期间就能看到并操作该现场。
			s.g.SetState(key, graph.NeedsAttention)
			res = s.parkForAttention(ctx, key, addr, termName, reportToken, dir, branch, updateActive, stopAndRelease, res.attention, &finish, &stopped)
		}
		if finish == finishStop && addr != "" && !stopped {
			if err := s.stopWorker(addr, termName); err != nil {
				finish = finishKeep
			} else {
				stopped = true
				s.runtime.clearAddr(key)
			}
		}
		switch finish {
		case finishKeep:
			// needs-attention 的 terminal 仍可能活着，但 fastimpl 已不再主动
			// 管理它。保留 metadata 供人看现场，同时明确标成不可自动恢复。
			// 若这个降级写不进去，宁可删除 metadata：留下 waiting/integrating
			// 会让下一次启动把一个本应人工检查的现场误当成可自动接管。
			if err := updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStageNeedsAttention }); err != nil && activeW != nil {
				if rmErr := activeW.remove(); rmErr != nil {
					s.log.Warn("remove unsafe active worker metadata failed", "key", key, "path", activePath, "err", rmErr)
				}
			}
			lock.Keep()
			s.keepBothLocks(key, token, lock.Path(), termName, addr)
			return
		case finishNone:
			// 没有可能存活的终端，按顺序回收两把锁。
		}
		if activeW != nil {
			if err := activeW.remove(); err != nil {
				s.log.Warn("remove active worker metadata failed", "key", key, "path", activePath, "err", err)
			}
		}
		if markerPosted {
			s.releaseMarker(ctx, key, token)
		}
		if err := lock.Release(); err != nil {
			s.log.Warn("release local task lock failed", "key", key, "lock", lock.Path(), "err", err)
		}
	}()

	s.log.Info("local task lock acquired", "key", key, "lock", lock.Path(), "recovered", recovered)
	taskCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if recovered {
		// 只读核验已经在偷锁前做过；这里重读最关键的现场，防止偷锁与
		// 接管之间 issue/claim 又变化。InFlight 是旧 supervisor 自己设置的，
		// 在 recovery 中不是拒绝条件。
		info, err := s.forge.IssueState(ctx, key.ProjectID, iid)
		if err != nil {
			fail(fmt.Errorf("re-read issue state for recovery: %w", err))
			return
		}
		if !info.Open {
			res.closed = true
			return
		}
		if !info.AssignedToMe || info.AssignedToOther {
			fail(fmt.Errorf("recovered issue assignment changed (me=%v other=%v)", info.AssignedToMe, info.AssignedToOther))
			return
		}
		if err := s.confirmClaim(ctx, ctx, key, token); err != nil {
			fail(fmt.Errorf("verify recovered claim after lock acquisition: %w", err))
			return
		}
		dir, branch = recovery.Worktree, recovery.Branch
		addr, termName = recovery.TerminalAddr, recovery.TerminalName
		s.log.Info("recovering active worker", "key", key, "terminal", termName, "addr", addr, "stage", recovery.Stage)
	} else {
		// 开跑前重读现场状态：建图快照可能已过期。
		info, err := s.forge.IssueState(ctx, key.ProjectID, iid)
		if err != nil {
			fail(fmt.Errorf("read issue state: %w", err))
			return
		}
		if !info.Open {
			res.closed = true
			return
		}
		if info.AssignedToOther {
			res.attention = s.noteFailure(ctx, key, "issue 已被他人领取，不抢占")
			return
		}
		if info.InFlight {
			res.attention = s.noteFailure(ctx, key, "issue 已在处理中（看板 status 为进行中），不抢占")
			return
		}

		// 评论仲裁：已有活跃认领就只告警，不发新评论、不改 issue 状态。
		winner, _, err := s.boundedRead(ctx, key, token)
		if err != nil {
			fail(fmt.Errorf("verify claim before starting: %w", err))
			return
		}
		if winner != nil {
			res.attention = claimBusy(winner).Error()
			s.log.Warn("issue already claimed elsewhere; not starting",
				"key", key, "instance", winner.instance, "claim", winner.token, "comment", winner.commentID)
			return
		}
		if err := s.postMarker(ctx, key, claimKindYes, token); err != nil {
			// POST 报错也可能已经落地：按同一 token 安全释放后放弃。
			s.log.Warn("post claim marker failed; releasing the same token", "key", key, "err", err)
			s.releaseMarker(ctx, key, token)
			fail(fmt.Errorf("post claim marker: %w", err))
			return
		}
		markerPosted = true
		updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStageClaimed })
		s.log.Info("claim marker posted", "key", key, "instance", s.cfg.InstanceID, "claim", token)
		if !s.waitClaimWindow(ctx) {
			fail(fmt.Errorf("wait for claim window: %w", ctx.Err()))
			return
		}
		if err := s.confirmClaim(ctx, ctx, key, token); err != nil {
			fail(fmt.Errorf("lose claim arbitration: %w", err))
			return
		}

		// 获胜后重读：仍开放、无他人/占用才真正指派。
		info, err = s.forge.IssueState(ctx, key.ProjectID, iid)
		if err != nil {
			fail(fmt.Errorf("re-read issue state: %w", err))
			return
		}
		if !info.Open {
			res.closed = true
			return
		}
		if info.AssignedToOther || info.InFlight {
			res.attention = "issue 已被他人领取或占用，不抢占"
			return
		}
		if !info.AssignedToMe {
			if err := s.forge.Claim(ctx, key.ProjectID, iid); err != nil {
				fail(fmt.Errorf("claim issue: %w", err))
				return
			}
		}
		// 指派后回读：必须是本人、无他人、无占用才算领取成功。
		info, err = s.forge.IssueState(ctx, key.ProjectID, iid)
		if err != nil {
			fail(fmt.Errorf("re-read issue state after claim: %w", err))
			return
		}
		if !info.Open {
			res.closed = true
			return
		}
		if !info.AssignedToMe || info.AssignedToOther || info.InFlight {
			fail(fmt.Errorf("claim not confirmed (open=%v me=%v other=%v in_flight=%v)",
				info.Open, info.AssignedToMe, info.AssignedToOther, info.InFlight))
			return
		}
		s.log.Info("issue claimed", "key", key)
		// 看板状态标记尽力而为：GitHub 未开 preview、GitLab CE/低版本
		// 都不支持，失败只记 warn 不阻塞任务。
		if err := s.forge.MarkInProgress(ctx, key.ProjectID, iid); err != nil {
			s.log.Warn("mark issue in progress failed", "key", key, "err", err)
		}
		dir, branch, err = s.repo.Create(ctx, s.branch, iid)
		if err != nil {
			fail(fmt.Errorf("create worktree: %w", err))
			return
		}
		updateActive(func(m *ActiveTaskMeta) {
			m.Worktree = dir
			m.Branch = branch
		})
		s.log.Info("worktree ready", "key", key, "dir", dir, "branch", branch)
		// 终端名 <repo>-issue-<iid>：多项目并行时不会撞名，
		// 且一眼能看出属于哪个仓库。
		termName = s.repo.Name + "-" + branch
		// 启动前最后确认一次：认领仍有效且任务未被取消，避免刚失锁就起终端。
		if err := s.confirmClaim(ctx, ctx, key, token); err != nil {
			fail(fmt.Errorf("verify claim before starting the worker: %w", err))
			return
		}
		// 从这里开始，任何失败都可能留下一个存活的终端。
		finish = finishKeep
		var reused bool
		addr, reused, err = s.claimRetainedTerminal(taskCtx, key, iid, false, termName, s.workDir, startSettings.AgentArgv)
		if err != nil {
			fail(err)
			return
		}
		if !reused {
			addr, err = s.sessions.StartWorker(taskCtx, worker.Params{
				SpecIID:  s.specIID,
				IssueIID: iid,
				Name:     termName,
				Worktree: dir,
				// agent 在外层工作区（work-dir）工作：其中可能还有它需要
				// 参考的主 checkout 与其他目录；worktree 只作产出目录，
				// 通过提示词告知。
				Cwd:  s.workDir,
				Argv: startSettings.AgentArgv,
			})
			if err != nil {
				// 启动结果不明确：可能已有终端，保留两把锁让人工确认。
				fail(fmt.Errorf("start worker: %w", err))
				s.log.Warn("start worker failed; keeping the local lock and the claim marker", "key", key, "terminal", termName, "err", err)
				return
			}
		}
	}

	if recovered {
		s.log.Info("worker recovered", "key", key, "terminal", termName, "addr", addr)
	} else {
		s.log.Info("worker running", "key", key, "terminal", termName, "addr", addr)
	}
	// 地址在 StartWorker/接管成功后、AwaitBoot 之前公开：GUI 要能连上一个
	// 还在启动、等着人处理权限弹窗的终端。
	s.runtime.setAddr(key, addr, termName, false)
	if recovered {
		startedAt := recovery.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		// 崩溃接管是同一轮工作：保留原开始时间，而不是从重启重新计时。
		s.runtime.setTimes(key, startedAt, time.Time{})
	} else {
		// 即使复用 retained terminal，也把这里视为本轮任务工作的开始；
		// 原完成时长已经在上一轮保留期间展示过，不应并入本轮工作时长。
		s.runtime.start(key, time.Now())
		startedAt, _ := s.runtime.times(key)
		updateActive(func(m *ActiveTaskMeta) { m.StartedAt = startedAt })
	}
	if err := lock.Update(lockContentWithTerminal(os.Getpid(), s.cfg.InstanceID, token, addr, termName)); err != nil {
		s.log.Warn("update task lock terminal metadata failed", "key", key, "lock", lock.Path(), "err", err)
	}
	if record, err := s.sessions.Snapshot(taskCtx, addr); err == nil {
		argv := record.Argv
		if len(argv) == 0 {
			argv = append([]string(nil), startSettings.AgentArgv...)
		}
		cwd := record.Cwd
		if cwd == "" {
			cwd = s.workDir
		}
		updateActive(func(m *ActiveTaskMeta) {
			m.TerminalAddr = addr
			m.TerminalName = termName
			m.TerminalCwd = cwd
			m.TerminalPID = record.PID
			m.TerminalArgv = append([]string(nil), argv...)
		})
	} else {
		updateActive(func(m *ActiveTaskMeta) {
			m.TerminalAddr = addr
			m.TerminalName = termName
			m.TerminalCwd = s.workDir
			m.TerminalArgv = append([]string(nil), startSettings.AgentArgv...)
		})
	}
	monitor := s.watchClaim(taskCtx, key, token, cancel)
	defer monitor.stop()
	// stopAndRelease 在任务成功整合后解除认领；terminal_retention 决定
	// 终端是立即停止，还是保留一段时间后由后台/下次启动清理。
	stopAndRelease = func(branch, commit string) error {
		monitor.stop()
		delay, never := s.retentionPolicy()
		if delay == 0 && !never {
			if err := s.stopWorker(addr, termName); err != nil {
				return err
			}
			stopped = true
			finish = finishNone
			s.runtime.clearAddr(key)
			return nil
		}
		stopped = true
		finish = finishNone
		s.retainTerminal(key, iid, false, addr, termName, s.workDir, branch, commit, startSettings.AgentArgv)
		return nil
	}

	baseline := uint64(0)
	attempts := 0
	needPrompt := true
	if recovered {
		baseline = recovery.PromptBaseline
		attempts = recovery.Attempts
		needPrompt = recovery.Stage == activeStageBooting
	}
	if needPrompt {
		// 终端已经启动/接管但 prompt 尚未发送：这就是 booting。必须先落盘，
		// 否则崩溃在 AwaitBoot 期间会被误标成 claimed，失去本可安全的无缝接管。
		if err := updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStageBooting }); err != nil {
			s.taskFailed(taskCtx, key, "record booting state", err, termName, &finish, &res, fail)
			return
		}
		s.log.Info("awaiting worker boot", "key", key, "addr", addr)
		if _, err := s.sessions.AwaitBoot(taskCtx, addr); err != nil {
			s.taskFailed(taskCtx, key, "await worker boot", err, termName, &finish, &res, fail)
			return
		}
		contextSection, contextPath, contextTruncated := s.issuePromptContext(key)
		promptSettings := s.currentRuntimeSettings(taskCtx)
		text, err := prompt.Render(promptSettings.Template, prompt.Vars{
			IssueNo:               iid,
			Token:                 reportToken,
			SpecNo:                s.specIID,
			Worktree:              dir,
			WorkDir:               s.workDir,
			RepoDir:               s.repo.Root,
			IntBranch:             s.branch,
			Branch:                branch,
			Forge:                 s.forgeName,
			ForgeCLI:              forgeCLI(s.forgeName),
			RequireReview:         promptSettings.RequireReview,
			Reviewer:              promptSettings.Reviewer,
			IssueContext:          contextSection,
			IssueContextPath:      contextPath,
			IssueContextTruncated: contextTruncated,
		})
		if err != nil {
			s.taskFailed(taskCtx, key, "render prompt", err, termName, &finish, &res, fail)
			return
		}
		text = issuecontext.EnsureInPrompt(text, contextSection)
		// 首次 Prompt 前明确重验自己的认领仍然有效。
		if err := s.confirmClaim(taskCtx, ctx, key, token); err != nil {
			s.taskFailed(taskCtx, key, "verify claim before prompt", err, termName, &finish, &res, fail)
			return
		}
		// Prompt 与 metadata 无法原子完成。先落 prompting：若崩溃发生在
		// 发送前后交界处，恢复方宁可阻塞人工检查，也绝不把可能已经送达的
		// prompt 再发一遍。这个状态写不进去时则干脆不发送。
		if err := updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStagePrompting }); err != nil {
			s.taskFailed(taskCtx, key, "record prompt transition", err, termName, &finish, &res, fail)
			return
		}
		baseline, err = s.sessions.Prompt(taskCtx, addr, text)
		if err != nil {
			s.taskFailed(taskCtx, key, "send prompt", err, termName, &finish, &res, fail)
			return
		}
		if err := updateActive(func(m *ActiveTaskMeta) {
			m.Stage = activeStageWaitingReport
			m.PromptBaseline = baseline
			m.Attempts = attempts
		}); err != nil {
			s.taskFailed(taskCtx, key, "record waiting-report state", err, termName, &finish, &res, fail)
			return
		}
		s.log.Info("prompt sent; waiting for report", "key", key)
	} else {
		s.log.Info("recovered worker prompt already in flight; waiting for report", "key", key, "baseline", baseline)
	}

	for {
		if _, err := s.sessions.WaitTurn(taskCtx, addr, baseline, worker.TurnBudget); err != nil {
			s.taskFailed(taskCtx, key, "wait worker turn", err, termName, &finish, &res, fail)
			return
		}
		if err := s.confirmClaim(taskCtx, ctx, key, token); err != nil {
			s.taskFailed(taskCtx, key, "verify claim before report", err, termName, &finish, &res, fail)
			return
		}
		_, repErr := s.captureReport(taskCtx, addr, reportToken)
		if repErr == nil {
			// 标记出现：提交以 worktree HEAD 为准，然后快速合并。
			commit, err := s.repo.HeadCommit(taskCtx, dir)
			if err != nil {
				s.taskFailed(taskCtx, key, "read HEAD of "+dir, err, termName, &finish, &res, fail)
				return
			}
			// 集成前再验一次：失锁后不能再合入/关 issue。
			if err := s.confirmClaim(taskCtx, ctx, key, token); err != nil {
				s.taskFailed(taskCtx, key, "verify claim before integrate", err, termName, &finish, &res, fail)
				return
			}
			updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStageIntegrating })
			err = s.tryMerge(taskCtx, branch, updateActive, recovery)
			if err == nil {
				if cause := context.Cause(taskCtx); cause != nil {
					s.taskFailed(taskCtx, key, "merged but claim lost", cause, termName, &finish, &res, fail)
					return
				}
				// Done 状态在 Close 之前：两端关闭 issue 都不会反向
				// 更新看板状态，不置 Done 会让已关闭 issue 永远停在
				// In progress。
				if merr := s.forge.MarkDone(taskCtx, key.ProjectID, iid); merr != nil {
					s.log.Warn("mark issue done failed", "key", key, "err", merr)
				}
				if err := s.forge.Close(taskCtx, key.ProjectID, iid); err != nil {
					s.taskFailed(taskCtx, key, "close issue", err, termName, &finish, &res, fail)
					return
				}
				// worktree 仍按原生命周期删除；retained terminal 主要用于回看
				// 输出/交互，不承诺保留任务工作区。
				if err := stopAndRelease(branch, commit); err != nil {
					fail(fmt.Errorf("stop worker %s: %w", termName, err))
					return
				}
				if err := s.repo.Remove(taskCtx, iid); err != nil {
					s.log.Warn("remove worktree failed", "key", key, "err", err)
				}
				res.integrated = true
				res.commit = commit
				return
			}
			var conflict *worktree.ConflictError
			if !errors.As(err, &conflict) {
				s.taskFailed(taskCtx, key, "integrate", err, termName, &finish, &res, fail)
				return
			}
			// 合并冲突：让 agent 在任务 worktree 解决后重试。
			if attempts >= s.currentRuntimeSettings(taskCtx).Retries {
				fail(fmt.Errorf("merge conflicts unresolved after %d retries: %s", attempts, conflict.Error()))
				return
			}
			attempts++
			s.log.Info("merge conflict; asking worker to resolve", "key", key, "files", conflict.Files)
			if err := s.confirmClaim(taskCtx, ctx, key, token); err != nil {
				s.taskFailed(taskCtx, key, "verify claim before conflict prompt", err, termName, &finish, &res, fail)
				return
			}
			if err := updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStagePrompting }); err != nil {
				s.taskFailed(taskCtx, key, "record conflict prompt transition", err, termName, &finish, &res, fail)
				return
			}
			baseline, err = s.sessions.Prompt(taskCtx, addr, conflictPrompt(dir, s.branch, conflict.Files, reportToken))
			if err != nil {
				s.taskFailed(taskCtx, key, "send conflict prompt", err, termName, &finish, &res, fail)
				return
			}
			if err := updateActive(func(m *ActiveTaskMeta) {
				m.Stage = activeStageWaitingReport
				m.PromptBaseline = baseline
				m.Attempts = attempts
			}); err != nil {
				s.taskFailed(taskCtx, key, "record conflict waiting state", err, termName, &finish, &res, fail)
				return
			}
			continue
		}
		// 没有完成标记：续写提示词重试，超过 retries 转人工。
		if attempts >= s.currentRuntimeSettings(taskCtx).Retries {
			fail(fmt.Errorf("no %s after %d retries: %v", worker.ReadyMarker, attempts, repErr))
			return
		}
		attempts++
		if err := s.confirmClaim(taskCtx, ctx, key, token); err != nil {
			s.taskFailed(taskCtx, key, "verify claim before continue prompt", err, termName, &finish, &res, fail)
			return
		}
		if err := updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStagePrompting }); err != nil {
			s.taskFailed(taskCtx, key, "record continue prompt transition", err, termName, &finish, &res, fail)
			return
		}
		baseline, err = s.sessions.Prompt(taskCtx, addr, worker.ContinuePrompt(reportToken, s.currentRuntimeSettings(taskCtx).RequireReview))
		if err != nil {
			s.taskFailed(taskCtx, key, "send continue prompt", err, termName, &finish, &res, fail)
			return
		}
		if err := updateActive(func(m *ActiveTaskMeta) {
			m.Stage = activeStageWaitingReport
			m.PromptBaseline = baseline
			m.Attempts = attempts
		}); err != nil {
			s.taskFailed(taskCtx, key, "record continue waiting state", err, termName, &finish, &res, fail)
			return
		}
	}
}

// taskFailed 归类一次失败：失锁或上游取消 → 停终端后释放；其它 →
// 保留终端与两把锁并写一条报告。
func (s *Scheduler) taskFailed(ctx context.Context, key graph.Key, what string, err error, termName string,
	finish *int, res *taskResult, fail func(error)) {
	cause := context.Cause(ctx)
	reason := fmt.Sprintf("%s: %v", what, err)
	switch {
	case errors.Is(err, ErrClaimLost), errors.Is(cause, ErrClaimLost):
		*finish = finishStop
		res.attention = fmt.Sprintf("%s (task cancelled: %v)", reason, cause)
		s.runtime.setAttention(key, res.attention)
		s.log.Warn("task stopped; the claim is no longer ours", "key", key, "reason", res.attention, "terminal", termName)
	case cause != nil:
		*finish = finishStop
		res.attention = fmt.Sprintf("%s (task cancelled: %v)", reason, cause)
		s.runtime.setAttention(key, res.attention)
		s.log.Warn("task stopped; parent context cancelled", "key", key, "reason", res.attention, "terminal", termName)
	default:
		s.log.Warn("task needs attention", "key", key, "err", err, "inspect", "hc attach "+termName)
		fail(fmt.Errorf("%s\n排查终端: hc attach %s", err.Error(), termName))
		res.attention = reason
		s.runtime.setAttention(key, res.attention)
	}
}

// noteFailure 把预检得到的长期阻塞原因写进 issue 评论（方便人工接手），
// 返回 attention 文案。认领竞争与本地锁不走这里，避免刷评论。
func (s *Scheduler) noteFailure(ctx context.Context, key graph.Key, reason string) string {
	nctx, cancel := s.cleanupCtx(ctx)
	defer cancel()
	if err := s.forge.Note(nctx, key.ProjectID, key.IID, "fastimpl: "+reason); err != nil {
		s.log.Warn("issue note failed", "key", key, "err", err)
	}
	s.runtime.setAttention(key, reason)
	return reason
}

func (s *Scheduler) cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), ClaimReadBudget)
}

// stopWorker 用独立且有界的 context 停掉本任务终端，不受已取消的任务
// context 影响，也不影响其他 worker。
func (s *Scheduler) stopWorker(addr, termName string) error {
	if addr == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), ClaimReadBudget)
	defer cancel()
	if err := s.sessions.Stop(ctx, addr, s.specIID, termName); err != nil {
		s.log.Warn("stop worker terminal failed", "addr", addr, "terminal", termName, "err", err)
		return err
	}
	return nil
}

// releaseMarker 尽力发 release 标记；失败只告警并给出人工处理指引。
func (s *Scheduler) releaseMarker(ctx context.Context, key graph.Key, token string) {
	rctx, cancel := s.cleanupCtx(ctx)
	defer cancel()
	if err := s.postMarker(rctx, key, claimKindNo, token); err != nil {
		s.log.Warn("release claim marker failed; the stale claim blocks other machines until removed by hand",
			"key", key, "instance", s.cfg.InstanceID, "claim", token,
			"action", "delete the "+claimLine(claimKindYes, s.cfg.InstanceID, token)+" comment")
		return
	}
	s.log.Info("claim marker released", "key", key, "claim", token)
}

// keepBothLocks 在无法确认 worker 已停止时保留本地锁与远端认领，
// 打印明确的清理指引。
func (s *Scheduler) keepBothLocks(key graph.Key, token, lockPath, termName, addr string) {
	s.log.Warn("could not confirm the worker stopped; keeping the local lock and the claim marker",
		"key", key, "terminal", termName, "addr", addr, "lock", lockPath,
		"instance", s.cfg.InstanceID, "claim", token,
		"action", "confirm that worker is stopped, then delete the local lock and the "+
			claimLine(claimKindYes, s.cfg.InstanceID, token)+" comment")
}

// waitClaimWindow 在发 claim 后短暂等待并允许 ctx 取消。
func (s *Scheduler) waitClaimWindow(ctx context.Context) bool {
	if s.claimWait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(s.claimWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func errClaimLost(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrClaimLost, fmt.Sprintf(format, args...))
}

// confirmClaim 在关键节点重验自己的认领仍然最早有效：读取用父 ctx 的
// 有界超时，读完后同时看任务 ctx 的 Cause（复查 goroutine 可能刚取消）。
func (s *Scheduler) confirmClaim(ctx, parent context.Context, key graph.Key, token string) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	rctx, cancel := context.WithTimeout(parent, ClaimReadBudget)
	defer cancel()
	winner, mine, err := s.claimState(rctx, key, token)
	if err != nil {
		return fmt.Errorf("verify claim: %w", err)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if holdsClaim(winner, mine) {
		return nil
	}
	if mine == nil {
		return errClaimLost("our claim marker is gone from issue comments")
	}
	return errClaimLost("instance %s claimed earlier (claim %s, comment %d)",
		winner.instance, winner.token, winner.commentID)
}

// Status 返回任务运行状态快照，供 CLI 状态行与 GUI 共用。
// 它由图快照与运行数据合成：每个节点一条，外加集成分支专属 agent
// 的一条（Integration 为真，Key 是 spec）。排序按 key，结果稳定。
// 状态与 close/in-flight 一律取 graph.Snapshot，运行里没有这些字段
// 的第二份真相；这里只补图上没有的终端地址、终端名、注意原因与
// 已接受提交。
func (s *Scheduler) Status() []TaskRuntime {
	live := s.runtime.snapshot()
	byKey := make(map[graph.Key]TaskRuntime, len(live))
	for _, r := range live {
		byKey[r.Key] = r
	}
	var out []TaskRuntime
	for _, n := range s.g.Snapshot() {
		r := byKey[n.Key]
		r.Key = n.Key
		out = append(out, r)
		delete(byKey, n.Key)
	}
	// 图里没有的条目（集成分支 agent 用 spec 的 key；已从图移除的
	// 节点也走这里）照原样补上，保证快照不漏。
	for _, r := range byKey {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b TaskRuntime) int {
		if a.Key.ProjectID != b.Key.ProjectID {
			return a.Key.ProjectID - b.Key.ProjectID
		}
		if a.Key.IID != b.Key.IID {
			return a.Key.IID - b.Key.IID
		}
		switch {
		case a.Integration == b.Integration:
			return 0
		case a.Integration:
			return -1
		}
		return 1
	})
	return out
}
