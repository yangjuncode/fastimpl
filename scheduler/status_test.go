package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// pauseSessions 把任务停在 AwaitBoot 上，直到测试放行：用来观察
// 地址在 StartWorker 之后、boot 之前的可见性。
type pauseSessions struct {
	*fakeSessions
	bootGate chan struct{}
}

func (p *pauseSessions) AwaitBoot(ctx context.Context, addr string) (worker.Record, error) {
	select {
	case <-p.bootGate:
	case <-ctx.Done():
		return worker.Record{}, ctx.Err()
	}
	return worker.Record{Live: true, Status: "idle", Reported: true}, nil
}

// statusOf 在快照里找 key 的条目。
func statusOf(s *Scheduler, k graph.Key) (TaskRuntime, bool) {
	for _, r := range s.Status() {
		if r.Key == k {
			return r, true
		}
	}
	return TaskRuntime{}, false
}

// waitFor 轮询条件，超时即失败。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStatusPublishesAddrBeforeBootAndClearsAfterStop(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})

	forge := &fakeForge{}
	sessions := &pauseSessions{fakeSessions: &fakeSessions{}, bootGate: make(chan struct{})}
	cfg := config.Config{Parallelism: 4, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, forge, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}",
		repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// 地址必须在 boot 之前公开：worker 卡在权限询问上时正是要人看的时候。
	key := graph.Key{ProjectID: 1, IID: 9}
	waitFor(t, "the task address before boot", func() bool {
		r, ok := statusOf(s, key)
		return ok && r.Addr != ""
	})
	r, _ := statusOf(s, key)
	if r.Label == "" {
		t.Error("status has no terminal label for the running task")
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 1 {
		t.Fatalf("workers started = %d, want 1", started)
	}

	// 放行后任务跑完：确认停止，地址随之清除，但名字与提交保留。
	close(sessions.bootGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	sessions.mu.Lock()
	stops := append([]string{}, sessions.stops...)
	sessions.mu.Unlock()
	if len(stops) != 1 || stops[0] != r.Addr {
		t.Fatalf("stops = %v, want the published address %q", stops, r.Addr)
	}
	r, ok := statusOf(s, key)
	if !ok {
		t.Fatal("status lost the task entry")
	}
	if r.Addr != "" {
		t.Errorf("status addr = %q after the stop was confirmed, want empty", r.Addr)
	}
	if r.Label == "" {
		t.Error("status lost the terminal label")
	}
	if r.Accepted == "" {
		t.Error("status has no accepted commit for the integrated task")
	}
	if r.Attention != "" {
		t.Errorf("status attention = %q for an integrated task, want empty", r.Attention)
	}
}

func TestStatusKeepsAddrWhenStopCannotBeConfirmed(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})

	forge := &fakeForge{}
	sessions := &fakeSessions{stopErr: errors.New("hc refused")}
	cfg := config.Config{Parallelism: 4, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, forge, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}",
		repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	// 停不掉终端 → 任务转人工，本次运行以停滞告终；这正是要留地址的情形。
	if err := s.Run(ctx); err == nil {
		t.Fatal("expected the run to stall after the terminal could not be stopped")
	}
	// 停不掉终端：地址留着，人工还能 hc attach 进去看。
	r, ok := statusOf(s, graph.Key{ProjectID: 1, IID: 9})
	if !ok || r.Addr == "" {
		t.Fatalf("status = %+v, want the address kept while the terminal is unconfirmed", r)
	}
}

func TestStatusRecordsAttention(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})

	// 他人已领取：#9 进 needs attention，原因要进快照。
	forge := &fakeForge{other: map[int]bool{9: true}}
	sessions := &fakeSessions{}
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, forge, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}",
		repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	if err := s.Run(ctx); err == nil {
		t.Fatal("expected the run to stall on the reserved task")
	}
	r, ok := statusOf(s, graph.Key{ProjectID: 1, IID: 9})
	if !ok {
		t.Fatal("status lost the task entry")
	}
	if r.Attention == "" {
		t.Error("status has no attention reason for a task someone else claimed")
	}
	if g.Snapshot()[0].State != graph.NeedsAttention {
		t.Errorf("graph state = %v, want needs attention", g.Snapshot()[0].State)
	}
}

func TestStatusIncludesNodesWithoutRuntime(t *testing.T) {
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 10}, Title: "b", Kind: graph.KindChild})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "a", Kind: graph.KindChild,
		Deps: []graph.Key{{ProjectID: 1, IID: 10}}})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 2, IID: 3}, Title: "ext", Kind: graph.KindExternal})
	s := &Scheduler{g: g}
	got := s.Status()
	want := []graph.Key{{ProjectID: 1, IID: 9}, {ProjectID: 1, IID: 10}, {ProjectID: 2, IID: 3}}
	if len(got) != len(want) {
		t.Fatalf("status = %+v, want %d entries", got, len(want))
	}
	for i, k := range want {
		if got[i].Key != k {
			t.Errorf("status[%d].Key = %v, want %v (key order)", i, got[i].Key, k)
		}
		if got[i].Addr != "" || got[i].Attention != "" {
			t.Errorf("status[%d] = %+v, want an empty runtime", i, got[i])
		}
	}
}

func TestStatusIntegrationAgent(t *testing.T) {
	repo := worktree.NewRepo("/repo", "/repo/.wt")
	repo.Name = "proj"
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	s := New(config.Config{InstanceID: testInstanceID}, repo, &fakeForge{}, "gitlab", g, &fakeSessions{}, "", "/work",
		"/repo/.wt/issue-8-integration", "issue-8-integration", "main", []string{"devin"}, slog.Default())
	// 集成 agent 用 spec 的 key 与独立标签，只在确认停止后才清除。
	s.runtime.setAddr(g.Spec(), "t7", s.intTermName(), true)
	got := s.Status()
	if len(got) != 1 {
		t.Fatalf("status = %+v, want the spec entry once", got)
	}
	if !got[0].Integration || got[0].Addr != "t7" || got[0].Label != "proj-issue-8-integration" {
		t.Fatalf("status = %+v, want the integration agent under the spec key", got[0])
	}
}

func TestRuntimeStoreConcurrentRefresh(t *testing.T) {
	var store runtimeStore
	key := graph.Key{ProjectID: 1, IID: 9}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				store.setAddr(key, fmt.Sprintf("t%d", i), "term", false)
				store.setAttention(key, fmt.Sprintf("why %d", i))
				_ = store.snapshot()
			}
		}()
	}
	wg.Wait()
	got := store.snapshot()
	if len(got) != 1 || got[0].Key != key || got[0].Addr == "" {
		t.Fatalf("snapshot = %+v, want one complete entry", got)
	}
}
