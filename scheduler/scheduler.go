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
	"strings"
	"sync"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/prompt"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// Tick 是调度循环查看图变化的间隔。
const Tick = 500 * time.Millisecond

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

// TaskRuntime 是调度器维护的每个任务的运行数据。
type TaskRuntime struct {
	// Key 是任务身份。
	Key graph.Key
	// Addr 是当前 worker 会话的 hc 终端地址；无运行时为空。
	Addr string
	// Attention 是进入 needs attention 的原因，供状态视图显示。
	Attention string
	// Accepted 是最近一次被接受的提交 SHA。
	Accepted string
}

// Scheduler 是分批并行调度的核心。
type Scheduler struct {
	cfg       config.Config
	repo      *worktree.Repo
	forge     forge.Forge
	g         *graph.Graph
	sessions  Sessions
	template  string
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
	intAddr   string // 集成专属 agent 终端地址；空表示未启动
	baseStuck bool   // base 冲突 agent 也未能解决，跳过后续同步
}

// New 组装调度器；所有依赖由 app 层注入，便于测试替换。
// intDir/intBranch/baseName 均来自 EnsureIntegration 的实际返回，
// 复用带描述后缀或配置覆盖的分支名时也能正确合并与推送。
func New(cfg config.Config, repo *worktree.Repo, f forge.Forge, g *graph.Graph, sessions Sessions, template, intDir, intBranch, baseName string, agentArgv []string, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		cfg:       cfg,
		repo:      repo,
		forge:     f,
		g:         g,
		sessions:  sessions,
		template:  template,
		intDir:    intDir,
		log:       log,
		specIID:   g.Spec().IID,
		branch:    intBranch,
		baseName:  baseName,
		agentArgv: agentArgv,
		warned:    make(map[graph.Key]bool),
	}
}

// taskResult 是一个任务 goroutine 的结局。
type taskResult struct {
	key        graph.Key
	integrated bool
	closed     bool // 开跑前发现 issue 已被外部关闭：不当失败，只标记
	commit     string
	attention  string
}

// Run 是调度主循环：就绪即派、完成即放，直到全部同项目工作项
// Closed/Integrated/Failed；无就绪且无在跑但图未完成时报停滞错误。
func (s *Scheduler) Run(ctx context.Context) error {
	defer s.stopIntegrationAgent()
	running := make(map[graph.Key]bool)
	results := make(chan taskResult, s.cfg.Parallelism)
	refresh := time.NewTicker(RefreshInterval)
	defer refresh.Stop()
	for {
		ready := s.g.Ready()
		for _, n := range ready {
			if len(running) >= s.cfg.Parallelism {
				break
			}
			if running[n.Key] {
				continue
			}
			running[n.Key] = true
			s.g.SetState(n.Key, graph.Running)
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
		select {
		case res := <-results:
			delete(running, res.key)
			switch {
			case res.integrated:
				s.g.SetState(res.key, graph.Integrated)
				s.log.Info("task integrated", "key", res.key, "commit", res.commit)
			case res.closed:
				s.g.SetClosed(res.key, true)
				s.log.Info("issue already closed; skipped", "key", res.key)
			default:
				s.g.SetState(res.key, graph.NeedsAttention)
				s.log.Warn("task needs attention", "key", res.key, "reason", res.attention)
			}
		case <-refresh.C:
			s.refreshStates(ctx)
		case <-time.After(Tick):
		case <-ctx.Done():
			return ctx.Err()
		}
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

// conflictPrompt 是让 agent 自己解决集成冲突的续写提示词：
// 在任务 worktree 中把集成分支合入本分支、解决冲突并提交，
// 然后输出报告标记；fastimpl 之后再做干净合并。
func conflictPrompt(intBranch string, files []string, token string) string {
	return fmt.Sprintf("你的分支与集成分支 %s 存在冲突（%s）。请在当前 worktree 中执行 git merge %s，逐个解决冲突并提交。冲突解决并提交完成后，在回复末尾单独一行输出 %s %s，随后最多六行说明解决方式；未解决前不要输出该标记。",
		intBranch, strings.Join(files, ", "), intBranch, worker.ReadyMarker, token)
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

// tryMerge 在串行锁内先把上游 base 同步进集成分支，
// 再合任务分支并推送。base 冲突交给集成分支专属 agent 解决；
// 解决不了则标记 baseStuck 跳过后续同步，任务合并照常进行，
// 冲突留给 MR/人工处理。
func (s *Scheduler) tryMerge(ctx context.Context, branch string) error {
	s.integrate.Lock()
	defer s.integrate.Unlock()
	if !s.baseStuck {
		err := s.repo.SyncBase(ctx, s.intDir, s.baseName)
		var conflict *worktree.ConflictError
		switch {
		case err == nil:
		case errors.As(err, &conflict):
			if rerr := s.resolveBaseConflict(ctx, conflict.Files); rerr != nil {
				s.baseStuck = true
				s.log.Warn("base conflict unresolved; skipping further sync",
					"base", s.baseName, "err", rerr, "inspect", "hc attach "+s.intTermName())
			}
		default:
			s.log.Warn("base sync failed; continuing", "base", s.baseName, "err", err)
		}
	}
	if err := s.repo.Merge(ctx, s.intDir, branch); err != nil {
		return err
	}
	return s.repo.Push(ctx, s.intDir, s.branch)
}

// resolveBaseConflict 让集成分支专属 agent 在集成 worktree 中解决
// origin/<base> 与集成分支的冲突：懒启动终端 → 发冲突提示词 →
// 等标记 → 重跑 SyncBase 验证干净。续写重试与任务共用 cfg.Retries。
// 调用方必须持有 integrate 锁——整个解决过程独占集成 worktree。
func (s *Scheduler) resolveBaseConflict(ctx context.Context, files []string) error {
	if s.intAddr == "" {
		termName := s.intTermName()
		addr, err := s.sessions.StartWorker(ctx, worker.Params{
			SpecIID:  s.specIID,
			IssueIID: s.specIID,
			Name:     termName,
			Worktree: s.intDir,
			Argv:     s.agentArgv,
		})
		if err != nil {
			return fmt.Errorf("start integration worker: %w", err)
		}
		if _, err := s.sessions.AwaitBoot(ctx, addr); err != nil {
			return fmt.Errorf("await integration worker boot: %w", err)
		}
		s.intAddr = addr
		s.log.Info("integration worker running", "terminal", termName, "addr", addr)
	}
	token := worker.NewToken()
	baseline, err := s.sessions.Prompt(ctx, s.intAddr, s.baseConflictPrompt(s.baseName, files, token))
	if err != nil {
		return fmt.Errorf("send base-conflict prompt: %w", err)
	}
	for attempts := 0; ; attempts++ {
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
		if attempts >= s.cfg.Retries {
			return fmt.Errorf("base merge conflict unresolved after %d retries", attempts)
		}
		baseline, err = s.sessions.Prompt(ctx, s.intAddr, worker.ContinuePrompt(token))
		if err != nil {
			return fmt.Errorf("send continue prompt: %w", err)
		}
	}
}

// stopIntegrationAgent 在 Run 结束时回收集成专属终端；
// base 冲突未解决时保留终端供 hc attach 排查。
func (s *Scheduler) stopIntegrationAgent() {
	if s.intAddr == "" {
		return
	}
	if s.baseStuck {
		s.log.Warn("integration worker left running for inspection",
			"inspect", "hc attach "+s.intTermName())
		return
	}
	if err := s.sessions.Stop(context.Background(), s.intAddr, s.specIID, s.intTermName()); err != nil {
		s.log.Warn("stop integration worker failed", "addr", s.intAddr, "err", err)
	}
}

// runTask 执行一个任务的完整生命周期，结局经 results 回报。
// 完成判定 = 屏幕上出现本任务令牌的 FASTIMPL-READY 行；
// 没有标记时按 cfg.Retries 向同一终端续写提示词重试；
// 合并冲突时让 agent 在任务 worktree 里解决后重试快速合并。
func (s *Scheduler) runTask(ctx context.Context, key graph.Key, results chan<- taskResult) {
	res := taskResult{key: key}
	iid := key.IID
	// report 把失败原因写到 issue 评论里再上报，方便人工接手。
	report := func(err error, note string) {
		if nerr := s.forge.Note(ctx, key.ProjectID, iid, note); nerr != nil {
			s.log.Warn("issue note failed", "key", key, "err", nerr)
		}
		res.attention = err.Error()
		results <- res
	}
	fail := func(err error) {
		report(err, "fastimpl: "+err.Error())
	}

	// 开跑前重读现场状态：建图快照可能已过期。
	info, err := s.forge.IssueState(ctx, key.ProjectID, iid)
	if err != nil {
		fail(fmt.Errorf("read issue state: %w", err))
		return
	}
	if !info.Open {
		// 外部已关闭：依赖视为满足，不启动 worker。
		res.closed = true
		results <- res
		return
	}
	if info.AssignedToOther {
		fail(errors.New("issue 已被他人领取，不抢占"))
		return
	}
	if info.InFlight {
		fail(errors.New("issue 已在处理中（看板 status 为进行中），不抢占"))
		return
	}
	if err := s.forge.Claim(ctx, key.ProjectID, iid); err != nil {
		fail(fmt.Errorf("claim issue: %w", err))
		return
	}
	// 看板状态标记尽力而为：GitHub 未开 preview、GitLab CE/低版本
	// 都不支持，失败只记 warn 不阻塞任务。
	if err := s.forge.MarkInProgress(ctx, key.ProjectID, iid); err != nil {
		s.log.Warn("mark issue in progress failed", "key", key, "err", err)
	}
	dir, branch, err := s.repo.Create(ctx, s.branch, iid)
	if err != nil {
		fail(fmt.Errorf("create worktree: %w", err))
		return
	}
	// 终端名 <repo>-issue-<iid>：多项目并行时不会撞名，
	// 且一眼能看出属于哪个仓库。
	termName := s.repo.Name + "-" + branch
	addr, err := s.sessions.StartWorker(ctx, worker.Params{
		SpecIID:  s.specIID,
		IssueIID: iid,
		Name:     termName,
		Worktree: dir,
		Argv:     s.agentArgv,
	})
	if err != nil {
		fail(fmt.Errorf("start worker: %w", err))
		return
	}
	s.log.Info("worker running", "key", key, "terminal", termName, "addr", addr)
	// worker 启动后的失败：保留终端让用户 hc attach 排查，不主动 Kill，
	// 且把排查入口写进 issue 评论。
	fail = func(err error) {
		s.log.Warn("task needs attention", "key", key, "err", err, "inspect", "hc attach "+termName)
		report(err, "fastimpl: "+err.Error()+"\n排查终端: hc attach "+termName)
	}

	if _, err := s.sessions.AwaitBoot(ctx, addr); err != nil {
		fail(fmt.Errorf("await worker boot: %w", err))
		return
	}
	token := worker.NewToken()
	text, err := prompt.Render(s.template, prompt.Vars{
		IssueNo:  iid,
		Token:    token,
		SpecNo:   s.specIID,
		Worktree: dir,
	})
	if err != nil {
		fail(fmt.Errorf("render prompt: %w", err))
		return
	}
	baseline, err := s.sessions.Prompt(ctx, addr, text)
	if err != nil {
		fail(fmt.Errorf("send prompt: %w", err))
		return
	}

	attempts := 0 // 首提示之外已用的续写次数
	for {
		if _, err := s.sessions.WaitTurn(ctx, addr, baseline, worker.TurnBudget); err != nil {
			fail(fmt.Errorf("wait worker turn: %w", err))
			return
		}
		_, repErr := s.captureReport(ctx, addr, token)
		if repErr == nil {
			// 标记出现：提交以 worktree HEAD 为准，然后快速合并。
			commit, err := s.repo.HeadCommit(ctx, dir)
			if err != nil {
				fail(fmt.Errorf("read HEAD of %s: %w", dir, err))
				return
			}
			err = s.tryMerge(ctx, branch)
			if err == nil {
				// Done 状态在 Close 之前：两端关闭 issue 都不会反向
				// 更新看板状态，不置 Done 会让已关闭 issue 永远停在
				// In progress。
				if merr := s.forge.MarkDone(ctx, key.ProjectID, iid); merr != nil {
					s.log.Warn("mark issue done failed", "key", key, "err", merr)
				}
				if err := s.forge.Close(ctx, key.ProjectID, iid); err != nil {
					fail(fmt.Errorf("close issue: %w", err))
					return
				}
				// 先退出 hc 进程再删 worktree：终端进程的 cwd 还在
				// worktree 里，先停掉才不会引用已删除的目录。
				if err := s.sessions.Stop(ctx, addr, s.specIID, termName); err != nil {
					s.log.Warn("stop worker terminal failed", "key", key, "addr", addr, "err", err)
				}
				if err := s.repo.Remove(ctx, iid); err != nil {
					s.log.Warn("remove worktree failed", "key", key, "err", err)
				}
				res.integrated = true
				res.commit = commit
				results <- res
				return
			}
			var conflict *worktree.ConflictError
			if !errors.As(err, &conflict) {
				fail(fmt.Errorf("integrate: %w", err))
				return
			}
			// 合并冲突：让 agent 在任务 worktree 解决后重试。
			if attempts >= s.cfg.Retries {
				fail(fmt.Errorf("merge conflicts unresolved after %d retries: %s", attempts, conflict.Error()))
				return
			}
			attempts++
			s.log.Info("merge conflict; asking worker to resolve", "key", key, "files", conflict.Files)
			baseline, err = s.sessions.Prompt(ctx, addr, conflictPrompt(s.branch, conflict.Files, token))
			if err != nil {
				fail(fmt.Errorf("send conflict prompt: %w", err))
				return
			}
			continue
		}
		// 没有完成标记：续写提示词重试，超过 retries 转人工。
		if attempts >= s.cfg.Retries {
			fail(fmt.Errorf("no %s after %d retries: %w", worker.ReadyMarker, attempts, repErr))
			return
		}
		attempts++
		baseline, err = s.sessions.Prompt(ctx, addr, worker.ContinuePrompt(token))
		if err != nil {
			fail(fmt.Errorf("send continue prompt: %w", err))
			return
		}
	}
}

// Status 返回任务运行状态快照，供 CLI 状态行与 GUI 共用。
// TODO: 汇总 graph.Snapshot + 每个 TaskRuntime。
func (s *Scheduler) Status() []TaskRuntime {
	return nil
}
