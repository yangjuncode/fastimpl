package scheduler

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
)

func runtimeFor(s *Scheduler, key graph.Key) (TaskRuntime, bool) {
	for _, r := range s.Status() {
		if r.Key == key {
			return r, true
		}
	}
	return TaskRuntime{}, false
}

func waitRetentionDone(t *testing.T, s *Scheduler) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.WaitRetention()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("retained terminal cleanup did not finish")
	}
}

// TestSuccessfulTaskDelayedRetention 验证成功任务不再立即停终端：锁与远端
// 认领仍立即释放，终端地址保持可连，后台定时器到期后才 Stop 并清 metadata。
func TestSuccessfulTaskDelayedRetention(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	base := &fakeSessions{}
	sessions := &commitSessions{fakeSessions: base, t: t}
	s, _, repoDir := runnableTask(t, f, sessions, nil)
	s.cfg.TerminalRetention = "40ms"
	key := graph.Key{ProjectID: 1, IID: 9}

	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	stops := append([]string{}, base.stops...)
	started := append([]worker.Params{}, base.started...)
	base.mu.Unlock()
	if len(stops) != 0 {
		t.Fatalf("stops immediately after Run = %v, want delayed terminal still live", stops)
	}
	if len(started) != 1 {
		t.Fatalf("started workers = %d, want 1", len(started))
	}
	if lockExists(repoDir, 9) {
		t.Fatal("local lock must be released even though the terminal is retained")
	}
	claims, releases, active := markerKinds(t, f, 9)
	if claims != 1 || releases != 1 || len(active) != 0 {
		t.Fatalf("claim=%d release=%d active=%+v, want remote claim released", claims, releases, active)
	}
	rt, ok := runtimeFor(s, key)
	if !ok || rt.Addr == "" {
		t.Fatalf("runtime for retained task missing addr: %+v ok=%v", rt, ok)
	}
	metaPath := retainedMetaPath(repoDir, 8, 9, false)
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("retained metadata missing: %v", err)
	}
	meta, err := readRetainedMeta(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if meta.StartedAt.IsZero() || meta.CompletedAt.IsZero() || meta.CompletedAt.Before(meta.StartedAt) {
		t.Fatalf("retained metadata times = %v .. %v, want a valid work interval", meta.StartedAt, meta.CompletedAt)
	}

	waitRetentionDone(t, s)
	base.mu.Lock()
	stops = append([]string{}, base.stops...)
	base.mu.Unlock()
	if len(stops) != 1 || stops[0] != rt.Addr {
		t.Fatalf("stops after retention delay = %v, want [%s]", stops, rt.Addr)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("retained metadata still exists after cleanup: %v", err)
	}
	rt, _ = runtimeFor(s, key)
	if rt.Addr != "" {
		t.Fatalf("runtime addr after cleanup = %q, want cleared", rt.Addr)
	}
}

// TestSuccessfulTaskNeverRetention 验证 never 不排任何自动清理；锁仍释放，
// metadata 留给下次启动接管/复用。
func TestSuccessfulTaskNeverRetention(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	base := &fakeSessions{}
	sessions := &commitSessions{fakeSessions: base, t: t}
	s, _, repoDir := runnableTask(t, f, sessions, nil)
	s.cfg.TerminalRetention = "never"

	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	stops := append([]string{}, base.stops...)
	base.mu.Unlock()
	if len(stops) != 0 {
		t.Fatalf("stops = %v, want never-retained terminal untouched", stops)
	}
	if s.RetentionPending() {
		t.Fatal("never retention must not create a cleanup timer")
	}
	if lockExists(repoDir, 9) {
		t.Fatal("local lock must be released for a completed retained task")
	}
	if _, err := os.Stat(retainedMetaPath(repoDir, 8, 9, false)); err != nil {
		t.Fatalf("retained metadata missing: %v", err)
	}
}

// TestRetainedTerminalReconcileAndReuse 直接覆盖重启接管的核心语义：
// 未到期的 retained terminal 重新排定时器；同一 issue 激活时取消清理、
// 删除 retained metadata，并默认复用原地址。
func TestRetainedTerminalReconcileAndReuse(t *testing.T) {
	taskLockDir(t)
	base := &fakeSessions{}
	s, _, repoDir := runnableTask(t, &fakeForge{}, base, nil)
	key := graph.Key{ProjectID: 1, IID: 9}
	termName := "-issue-9" // runnableTask 未设置 repo.Name，生产环境是 <repo>-issue-<iid>
	addr := "t-retained"
	base.records = map[string]worker.Record{
		addr: {Live: true, Status: "idle", Reported: true, Name: termName, Cwd: CanonicalRepoPath(repoDir), Argv: []string{"devin"}, Labels: map[string]string{
			"fastimpl": "8", "task": termName,
		}},
	}
	path := retainedMetaPath(repoDir, 8, 9, false)
	started := time.Now().Add(-time.Hour).Truncate(time.Second)
	completed := started.Add(7 * time.Minute)
	meta := retainedTerminalMeta{
		Version: 1, RepoDir: CanonicalRepoPath(repoDir), SpecIID: 8, IssueIID: 9,
		TerminalAddr: addr, TerminalName: termName, Cwd: CanonicalRepoPath(repoDir),
		Branch: "issue-9", AgentHash: agentArgvHash([]string{"devin"}),
		StartedAt: started, CompletedAt: completed, CleanupAt: time.Now().Add(time.Hour),
	}
	if err := writeRetainedMeta(path, meta); err != nil {
		t.Fatal(err)
	}

	s.reconcileRetained(context.Background())
	if !s.RetentionPending() {
		t.Fatal("unexpired retained terminal must be re-scheduled after restart")
	}
	rt, ok := runtimeFor(s, key)
	if !ok || rt.Addr != addr {
		t.Fatalf("retained terminal not re-exposed to GUI: %+v ok=%v", rt, ok)
	}
	if !rt.StartedAt.Equal(started) || !rt.FinishedAt.Equal(completed) {
		t.Fatalf("restored retained times = %v .. %v, want %v .. %v", rt.StartedAt, rt.FinishedAt, started, completed)
	}
	got, reused, err := s.claimRetainedTerminal(context.Background(), key, 9, false, termName, repoDir, []string{"devin"})
	if err != nil {
		t.Fatal(err)
	}
	if !reused || got != addr {
		t.Fatalf("claimRetainedTerminal = %q,%v, want reused %s", got, reused, addr)
	}
	if s.RetentionPending() {
		t.Fatal("reactivating a retained terminal must cancel its cleanup timer")
	}
	rt, _ = runtimeFor(s, key)
	if !rt.StartedAt.After(started) || !rt.FinishedAt.IsZero() {
		t.Fatalf("reactivated times = %v .. %v, want a fresh running interval", rt.StartedAt, rt.FinishedAt)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("active terminal must not keep retained metadata: %v", err)
	}
}

// TestClaimExpiredRetainedTerminalCleansBeforeStart 过期 terminal 在重启接管后
// 不再复用；同一 issue 准备重跑时会先按原保留承诺清掉它。
func TestClaimExpiredRetainedTerminalCleansBeforeStart(t *testing.T) {
	taskLockDir(t)
	base := &fakeSessions{}
	s, _, repoDir := runnableTask(t, &fakeForge{}, base, nil)
	key := graph.Key{ProjectID: 1, IID: 9}
	termName := "-issue-9"
	addr := "t-expired"
	base.records = map[string]worker.Record{
		addr: {Live: true, Status: "idle", Reported: true, Name: termName, Cwd: CanonicalRepoPath(repoDir), Argv: []string{"devin"}, Labels: map[string]string{
			"fastimpl": "8", "task": termName,
		}},
	}
	path := retainedMetaPath(repoDir, 8, 9, false)
	meta := retainedTerminalMeta{
		Version: 1, RepoDir: CanonicalRepoPath(repoDir), SpecIID: 8, IssueIID: 9,
		TerminalAddr: addr, TerminalName: termName, Cwd: CanonicalRepoPath(repoDir),
		AgentHash: agentArgvHash([]string{"devin"}), CompletedAt: time.Now().Add(-2 * time.Hour),
		CleanupAt: time.Now().Add(-time.Hour),
	}
	if err := writeRetainedMeta(path, meta); err != nil {
		t.Fatal(err)
	}
	got, reused, err := s.claimRetainedTerminal(context.Background(), key, 9, false, termName, repoDir, []string{"devin"})
	if err != nil {
		t.Fatal(err)
	}
	if reused || got != "" {
		t.Fatalf("expired terminal reused as %q,%v; want cleanup then fresh start", got, reused)
	}
	base.mu.Lock()
	stops := append([]string{}, base.stops...)
	base.mu.Unlock()
	if len(stops) != 1 || stops[0] != addr {
		t.Fatalf("stops = %v, want expired terminal %s stopped", stops, addr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired retained metadata still exists: %v", err)
	}
}

// retained terminal 的名字/labels 可以被误用或重建；真正复用前还必须核对
// hc 记录里的实际 cwd 与 agent argv。否则同 issue 重跑可能把 prompt 送进
// 一个只“看起来像”的错误终端。
func TestClaimRetainedTerminalRejectsProcessIdentityMismatch(t *testing.T) {
	taskLockDir(t)
	base := &fakeSessions{}
	s, _, repoDir := runnableTask(t, &fakeForge{}, base, nil)
	key := graph.Key{ProjectID: 1, IID: 9}
	termName := "-issue-9"
	addr := "t-retained-mismatch"
	base.records = map[string]worker.Record{
		addr: {Live: true, Status: "idle", Reported: true, Name: termName, PID: 7001,
			Cwd: repoDir + "/elsewhere", Argv: []string{"devin"}, Labels: map[string]string{
				"fastimpl": "8", "task": termName,
			}},
	}
	path := retainedMetaPath(repoDir, 8, 9, false)
	meta := retainedTerminalMeta{
		Version: 1, RepoDir: CanonicalRepoPath(repoDir), SpecIID: 8, IssueIID: 9,
		TerminalAddr: addr, TerminalName: termName, Cwd: CanonicalRepoPath(repoDir),
		AgentHash: agentArgvHash([]string{"devin"}), CompletedAt: time.Now().Add(-time.Minute),
		CleanupAt: time.Now().Add(time.Hour),
	}
	if err := writeRetainedMeta(path, meta); err != nil {
		t.Fatal(err)
	}
	got, reused, err := s.claimRetainedTerminal(context.Background(), key, 9, false, termName, repoDir, []string{"devin"})
	if err == nil || reused || got != "" {
		t.Fatalf("claimRetainedTerminal = %q,%v,%v, want process identity mismatch rejected", got, reused, err)
	}
}
