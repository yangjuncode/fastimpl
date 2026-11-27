package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// uncertainPromptSessions 模拟真实事故：文本已经写入终端（fake 已记录 prompt，
// 后续 ReadScreen 能看到 READY），但启动确认失败，旧实现会立刻放弃监督。
type uncertainPromptSessions struct {
	*fakeSessions
	committer *commitSessions
	failed    bool
}

func (u *uncertainPromptSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	if u.committer != nil {
		return u.committer.StartWorker(ctx, p)
	}
	return u.fakeSessions.StartWorker(ctx, p)
}

func (u *uncertainPromptSessions) Prompt(ctx context.Context, addr, text string) (uint64, error) {
	if u.failed {
		return u.fakeSessions.Prompt(ctx, addr, text)
	}
	u.failed = true
	if _, err := u.fakeSessions.Prompt(ctx, addr, text); err != nil {
		return 0, err
	}
	return 0, errors.New("worker terminal did not accept the prompt")
}

// TestAttentionConfirmCompleteIntegratesAndReleasesDependency 锁住用户事故：
// prompt 实际已送达且 worker 完成，但首次发送确认失败。交互式调度不能把
// 任务丢在世界外；监督发现报告后，人工确认应继续集成并释放下游依赖。
func TestAttentionConfirmCompleteIntegratesAndReleasesDependency(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	base := &fakeSessions{}
	sessions := &uncertainPromptSessions{fakeSessions: base, committer: &commitSessions{fakeSessions: base, t: t}}
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, repoDir+"/.wt")
	intBranch, intDir, baseName, err := repo.EnsureIntegration(context.Background(), 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	key9 := graph.Key{ProjectID: 1, IID: 9}
	key10 := graph.Key{ProjectID: 1, IID: 10}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: key9, Title: "task A", Kind: graph.KindChild})
	g.Add(graph.Node{Key: key10, Title: "task B", Kind: graph.KindChild, Deps: []graph.Key{key9}})
	s := newTestScheduler(config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}, repo, f, "gitlab", g, sessions,
		"{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, nil)
	s.SetInteractiveAttention(true)
	s.attentionPoll = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	waitFor(t, "task parked in needs attention with a detected report", func() bool {
		n, ok := g.Lookup(key9)
		rt, rtOK := statusOf(s, key9)
		return ok && n.State == graph.NeedsAttention && rtOK && rt.ReportReady
	})
	select {
	case err := <-runErr:
		t.Fatalf("interactive Run exited while the attention task was recoverable: %v", err)
	default:
	}

	if _, err := s.ControlNode(ctx, key9, NodeActionConfirmComplete); err != nil {
		t.Fatalf("confirm complete: %v", err)
	}
	waitFor(t, "dependent task dispatched after confirmation", func() bool {
		n, ok := g.Lookup(key10)
		return ok && (n.State == graph.Running || n.State == graph.Integrated || n.Closed)
	})
	cancel()
	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	n, _ := g.Lookup(key9)
	if n.State != graph.Integrated && !n.Closed {
		t.Fatalf("node state after confirm = %v closed=%v, want integrated/closed", n.State, n.Closed)
	}
}

// TestAttentionRestartStopsTerminalAndRedispatches 锁住“重新调度”：停止旧终端、
// 释放旧认领/锁后节点回到 Pending，调度器能再次派发同一 issue。
func TestAttentionRestartStopsTerminalAndRedispatches(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	base := &fakeSessions{}
	sessions := &uncertainPromptSessions{fakeSessions: base}
	s, g, repoDir := runnableTask(t, f, sessions, nil)
	key := graph.Key{ProjectID: 1, IID: 9}
	s.SetInteractiveAttention(true)
	s.attentionPoll = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	waitFor(t, "task parked in needs attention", func() bool {
		n, ok := g.Lookup(key)
		return ok && n.State == graph.NeedsAttention
	})
	base.mu.Lock()
	firstAddr := base.stops
	startsBefore := len(base.started)
	base.mu.Unlock()
	if len(firstAddr) != 0 || startsBefore != 1 {
		t.Fatalf("before restart stops=%v starts=%d, want no stop and one start", firstAddr, startsBefore)
	}

	if _, err := s.ControlNode(ctx, key, NodeActionRestart); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitFor(t, "worker redispatched after restart", func() bool {
		base.mu.Lock()
		defer base.mu.Unlock()
		return len(base.started) >= 2
	})
	cancel()
	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if !lockExists(repoDir, 9) {
		return
	}
	// 取消时第二个 worker 的收尾会释放它自己的锁；若仍看到锁，说明重启路径
	// 没有按预期清理/接管，测试需要失败而不是静默放过。
	t.Fatal("task lock still exists after cancellation cleanup")
}
