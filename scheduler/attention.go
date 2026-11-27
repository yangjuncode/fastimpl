package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
)

// NodeAction 是 GUI/人工对单个图节点执行的控制动作。
type NodeAction string

const (
	NodeActionRefresh         NodeAction = "refresh"
	NodeActionConfirmComplete NodeAction = "confirm_complete"
	NodeActionStop            NodeAction = "stop"
	NodeActionCancel          NodeAction = "cancel"
	NodeActionRestart         NodeAction = "restart"
)

type attentionControl struct {
	key     graph.Key
	ch      chan NodeAction
	confirm chan struct{}
	done    chan struct{}
}

// SetInteractiveAttention 打开人工接管模式：失败后任务 goroutine 继续监督
// 终端并等待 GUI 控制，而不是让调度器直接 stall。只影响尚未失败的节点。
func (s *Scheduler) SetInteractiveAttention(enabled bool) {
	s.interactiveAttention.Store(enabled)
}

func (s *Scheduler) registerAttentionControl(key graph.Key, c *attentionControl) {
	s.attentionMu.Lock()
	s.attentionControls[key] = c
	s.attentionMu.Unlock()
}

func (s *Scheduler) unregisterAttentionControl(key graph.Key, c *attentionControl) {
	s.attentionMu.Lock()
	if s.attentionControls[key] == c {
		delete(s.attentionControls, key)
	}
	s.attentionMu.Unlock()
}

func (s *Scheduler) attentionControlFor(key graph.Key) *attentionControl {
	s.attentionMu.Lock()
	defer s.attentionMu.Unlock()
	return s.attentionControls[key]
}

// ControlNode 执行节点控制。除 refresh 外，动作都由该任务的 goroutine 串行执行，
// 避免 GUI 与 worker 生命周期并发地释放锁、claim 或 active metadata。
func (s *Scheduler) ControlNode(ctx context.Context, key graph.Key, action NodeAction) (string, error) {
	switch action {
	case NodeActionRefresh:
		return s.refreshAttentionNode(ctx, key)
	case NodeActionConfirmComplete:
		c := s.attentionControlFor(key)
		if c == nil {
			return "", fmt.Errorf("节点 %v 当前没有等待人工确认的现场", key)
		}
		select {
		case c.confirm <- struct{}{}:
			return "已确认 worker 报告，开始集成", nil
		case <-c.done:
			return "", fmt.Errorf("节点 %v 的现场已结束，请刷新后重试", key)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	case NodeActionStop, NodeActionCancel, NodeActionRestart:
		c := s.attentionControlFor(key)
		if c == nil {
			return "", fmt.Errorf("节点 %v 当前没有可控制的运行现场", key)
		}
		select {
		case c.ch <- action:
			return "控制请求已提交", nil
		case <-c.done:
			return "", fmt.Errorf("节点 %v 的现场已结束，请刷新后重试", key)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", fmt.Errorf("未知节点操作 %q", action)
}

// refreshAttentionNode 重新读取 issue 与终端现场。它是安全只读操作；若发现
// worker 报告已就绪，只更新界面标记，是否集成仍由人工确认决定。
func (s *Scheduler) refreshAttentionNode(ctx context.Context, key graph.Key) (string, error) {
	if err := s.refreshIssueState(ctx, key); err != nil {
		return "", err
	}
	n, ok := s.g.Lookup(key)
	if !ok {
		return "", fmt.Errorf("节点 %v 不在当前依赖图中", key)
	}
	rt, _ := s.runtime.lookup(key)
	if n.State != graph.NeedsAttention {
		return fmt.Sprintf("节点 %v 已刷新：状态=%s，无需 attention 处理", key, n.State), nil
	}
	addr := rt.Addr
	if addr == "" {
		if meta, err := readActiveMeta(ActiveMetaPath(s.repo.Root, key.IID)); err == nil {
			addr = meta.TerminalAddr
		}
	}
	if addr == "" {
		return fmt.Sprintf("节点 %v 已刷新：仍处 needs attention；找不到终端地址", key), nil
	}
	snapCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	snap, err := s.sessions.Snapshot(snapCtx, addr)
	cancel()
	if err != nil {
		return "", fmt.Errorf("读取终端 %s 状态失败: %w", addr, err)
	}
	if !snap.Live {
		return fmt.Sprintf("节点 %v 已刷新：终端 %s 已退出；可重新调度", key, addr), nil
	}
	reportCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	screen, complete, err := s.sessions.ReadScreen(reportCtx, addr, worker.CaptureLines)
	cancel()
	token := ""
	if meta, metaErr := readActiveMeta(ActiveMetaPath(s.repo.Root, key.IID)); metaErr == nil {
		token = meta.ReportToken
	}
	if err == nil && complete && token != "" && workerContainsReport(screen, token) {
		s.runtime.setReportReady(key, true)
		return fmt.Sprintf("节点 %v 已刷新：检测到 worker 完成报告，可人工确认完成", key), nil
	}
	return fmt.Sprintf("节点 %v 已刷新：终端存活，状态=%s，turn=%d", key, snap.Status, snap.Turn), nil
}

func (s *Scheduler) refreshIssueState(ctx context.Context, key graph.Key) error {
	info, err := s.forge.IssueState(ctx, key.ProjectID, key.IID)
	if err != nil {
		return err
	}
	old, ok := s.g.Lookup(key)
	if !ok {
		return fmt.Errorf("节点 %v 不在当前依赖图中", key)
	}
	if old.Closed == info.Open {
		s.g.SetClosed(key, !info.Open)
		s.log.Info("refreshed issue state", "key", key, "open", info.Open)
	}
	return nil
}

// parkForAttention 是普通失败后的交互式监督循环。它继续使用原 terminal 的
// 地址观察 report；真正会改写状态的 confirm/restart/cancel 都在本 goroutine
// 内完成，保持与原 runTask 清理路径同一个执行者。finish/stopped 是 runTask
// 的收尾变量，动作可直接决定最终清理方式。
func (s *Scheduler) parkForAttention(ctx context.Context, key graph.Key, terminalAddr, termName, reportToken, dir, branch string,
	updateActive mergeProgress, stopAndRelease func(branch, commit string) error, attention string, finish *int, stopped *bool) taskResult {
	control := &attentionControl{key: key, ch: make(chan NodeAction, 4), confirm: make(chan struct{}, 1), done: make(chan struct{})}
	s.registerAttentionControl(key, control)
	defer func() {
		s.unregisterAttentionControl(key, control)
		close(control.done)
	}()
	poll := s.attentionPoll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	s.monitorAttentionTerminal(ctx, key, terminalAddr, reportToken)
	for {
		select {
		case <-ctx.Done():
			return taskResult{key: key, attention: attention}
		case <-ticker.C:
			s.monitorAttentionTerminal(ctx, key, terminalAddr, reportToken)
		case action := <-control.ch:
			switch action {
			case NodeActionStop:
				if s.stopAttentionTerminal(key, terminalAddr, termName) {
					attention += "；终端已人工停止，可重新调度"
					s.runtime.setAttention(key, attention)
				}
			case NodeActionCancel:
				if !s.stopAttentionTerminal(key, terminalAddr, termName) {
					continue
				}
				*stopped = true
				*finish = finishNone
				return taskResult{key: key, attention: attention + "；已人工取消"}
			case NodeActionRestart:
				if !s.stopAttentionTerminal(key, terminalAddr, termName) {
					continue
				}
				*stopped = true
				*finish = finishNone
				return taskResult{key: key, restart: true}
			}
		case <-control.confirm:
			if !s.confirmAttentionComplete(ctx, key, terminalAddr, reportToken) {
				continue
			}
			if dir == "" || branch == "" || updateActive == nil || stopAndRelease == nil {
				s.runtime.setAttention(key, "现场缺少 worktree/branch 或收尾上下文，不能安全确认完成")
				continue
			}
			commit, err := s.repo.HeadCommit(ctx, dir)
			if err != nil {
				attention = fmt.Sprintf("read HEAD of %s: %v", dir, err)
				s.runtime.setAttention(key, attention)
				continue
			}
			if err := updateActive(func(m *ActiveTaskMeta) { m.Stage = activeStageIntegrating }); err != nil {
				s.runtime.setAttention(key, err.Error())
				continue
			}
			if err := s.tryMerge(ctx, branch, updateActive, nil); err != nil {
				attention = err.Error()
				s.runtime.setAttention(key, attention)
				continue
			}
			if merr := s.forge.MarkDone(ctx, key.ProjectID, key.IID); merr != nil {
				s.log.Warn("mark issue done failed", "key", key, "err", merr)
			}
			if err := s.forge.Close(ctx, key.ProjectID, key.IID); err != nil {
				attention = fmt.Sprintf("close issue: %v", err)
				s.runtime.setAttention(key, attention)
				continue
			}
			if err := stopAndRelease(branch, commit); err != nil {
				attention = fmt.Sprintf("stop worker %s: %v", termName, err)
				s.runtime.setAttention(key, attention)
				continue
			}
			if err := s.repo.Remove(ctx, key.IID); err != nil {
				s.log.Warn("remove worktree failed", "key", key, "err", err)
			}
			return taskResult{key: key, integrated: true, commit: commit}
		}
	}
}

func (s *Scheduler) monitorAttentionTerminal(ctx context.Context, key graph.Key, terminalAddr, reportToken string) {
	if terminalAddr == "" {
		return
	}
	reportCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	screen, complete, err := s.sessions.ReadScreen(reportCtx, terminalAddr, worker.CaptureLines)
	cancel()
	if err != nil || !complete || reportToken == "" {
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Debug("attention terminal read failed", "key", key, "terminal", terminalAddr, "error", err)
		}
		return
	}
	if workerContainsReport(screen, reportToken) {
		if rt, ok := s.runtime.lookup(key); ok && !rt.ReportReady {
			s.log.Info("worker completion report detected while needs attention", "key", key, "terminal", terminalAddr)
		}
		s.runtime.setReportReady(key, true)
	}
}

func (s *Scheduler) confirmAttentionComplete(ctx context.Context, key graph.Key, terminalAddr, reportToken string) bool {
	if rt, ok := s.runtime.lookup(key); ok && rt.ReportReady {
		return true
	}
	s.monitorAttentionTerminal(ctx, key, terminalAddr, reportToken)
	if rt, ok := s.runtime.lookup(key); ok && rt.ReportReady {
		return true
	}
	s.runtime.setAttention(key, "尚未在该终端看到本轮完成报告，不能确认完成；请先刷新或检查终端")
	return false
}

func (s *Scheduler) stopAttentionTerminal(key graph.Key, terminalAddr, termName string) bool {
	if terminalAddr == "" {
		return true
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.sessions.Stop(stopCtx, terminalAddr, s.specIID, termName); err != nil {
		s.log.Warn("manual stop terminal failed", "key", key, "terminal", terminalAddr, "error", err)
		return false
	}
	s.log.Info("terminal stopped by manual control", "key", key, "terminal", terminalAddr)
	return true
}

func workerContainsReport(screen, token string) bool {
	_, err := worker.ParseReport(screen, token)
	return err == nil
}
