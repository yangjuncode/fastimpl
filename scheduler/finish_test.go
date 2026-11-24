package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// runnableTask 组装一个只有一个就绪子任务的调度器，供收尾语义测试复用。
func runnableTask(t *testing.T, f forge.Forge, sessions Sessions, log *slog.Logger) (*Scheduler, *graph.Graph, string) {
	t.Helper()
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	if log == nil {
		log = slog.Default()
	}
	s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions,
		"{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, log)
	return s, g, repoDir
}

// lockFiles 返回该仓库/issue 的本地锁是否还在。
func lockExists(repoDir string, iid int) bool {
	_, err := os.Stat(TaskLockPath(repoDir, iid))
	return err == nil
}

// markerKinds 统计 issue 上 claim/release 标记的条数与是否存在自己的
// 活跃认领。
func markerKinds(t *testing.T, f *fakeForge, iid int) (claims, releases int, active []activeClaim) {
	t.Helper()
	f.mu.Lock()
	comments := append([]forge.IssueComment{}, f.comments[iid]...)
	f.mu.Unlock()
	for _, c := range comments {
		if kind, _, _, ok := parseClaim(c.Body); ok {
			if kind == claimKindYes {
				claims++
			} else {
				releases++
			}
		}
	}
	active, err := activeClaims(comments)
	if err != nil {
		t.Fatalf("activeClaims: %v", err)
	}
	return claims, releases, active
}

// TestRunTaskStartWorkerErrorKeepsBothLocks 启动调用失败、拿不到地址时，
// 结果不明：本地锁与远端认领都保留，不能假报已释放。
func TestRunTaskStartWorkerErrorKeepsBothLocks(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := &fakeSessions{startErr: errors.New("hc connect refused")}
	s, _, repoDir := runnableTask(t, f, sessions, nil)

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error")
	}
	if !lockExists(repoDir, 9) {
		t.Fatal("local lock must be kept when the worker start result is unknown")
	}
	claims, releases, active := markerKinds(t, f, 9)
	if claims != 1 || releases != 0 {
		t.Fatalf("markers claim=%d release=%d, want the claim kept and not released", claims, releases)
	}
	if len(active) != 1 {
		t.Fatalf("active claims = %+v, want our claim preserved", active)
	}
	sessions.mu.Lock()
	stops := append([]string{}, sessions.stops...)
	sessions.mu.Unlock()
	if len(stops) != 0 {
		t.Fatalf("stops = %v, want none (no address to stop)", stops)
	}
}

// TestRunTaskWorkerErrorKeepsTerminal 普通运行失败：保留终端、本地锁与
// 认领，写一条报告。
func TestRunTaskWorkerErrorKeepsTerminal(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := &fakeSessions{}
	s, _, repoDir := runnableTask(t, f, &failingBootSessions{fakeSessions: sessions}, nil)

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error")
	}
	if !lockExists(repoDir, 9) {
		t.Fatal("local lock must be kept for a failed worker (terminal left for inspection)")
	}
	claims, releases, active := markerKinds(t, f, 9)
	if claims != 1 || releases != 0 || len(active) != 1 {
		t.Fatalf("claim=%d release=%d active=%+v, want the claim kept", claims, releases, active)
	}
	sessions.mu.Lock()
	stops := append([]string{}, sessions.stops...)
	sessions.mu.Unlock()
	if len(stops) != 0 {
		t.Fatalf("stops = %v, want the failed terminal left running", stops)
	}
	f.mu.Lock()
	notes := append([]string{}, f.notes...)
	f.mu.Unlock()
	found := false
	for _, n := range notes {
		if strings.Contains(n, "排查终端") && !strings.Contains(n, claimVersion) {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes = %v, want one failure report with the attach hint", notes)
	}
}

// failingBootSessions 让 AwaitBoot 失败，模拟 worker 启动后无法就绪。
type failingBootSessions struct{ *fakeSessions }

func (f *failingBootSessions) AwaitBoot(ctx context.Context, addr string) (worker.Record, error) {
	return worker.Record{}, errors.New("worker never reported idle")
}

// TestRunTaskClaimLostStopsTerminal 关键核验发现失锁：停终端、不关 issue、
// 释放两把锁。
func TestRunTaskClaimLostStopsTerminal(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := &fakeSessions{}
	s, _, repoDir := runnableTask(t, f, &loseClaimSessions{fakeSessions: sessions, forge: f}, nil)

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error")
	}
	if lockExists(repoDir, 9) {
		t.Fatal("local lock must be released once the terminal was stopped")
	}
	sessions.mu.Lock()
	stops := append([]string{}, sessions.stops...)
	sessions.mu.Unlock()
	if len(stops) != 1 {
		t.Fatalf("stops = %v, want exactly one stop for the lost claim", stops)
	}
	f.mu.Lock()
	closed := append([]graph.Key{}, f.closed...)
	f.mu.Unlock()
	if len(closed) != 0 {
		t.Fatalf("closed = %v, want none (lost claim must not close the issue)", closed)
	}
	_, releases, active := markerKinds(t, f, 9)
	if releases != 1 {
		t.Fatalf("release markers = %d, want our marker released", releases)
	}
	for _, a := range active {
		if a.instance == testInstanceID {
			t.Fatalf("our claim is still active: %+v (want it released after stopping)", a)
		}
	}
}

// loseClaimSessions 在 worker 启动后插入一条更早的认领，让关键的同步
// 核验直接失败（不必等复查 goroutine）。
type loseClaimSessions struct {
	*fakeSessions
	forge *fakeForge
}

func (l *loseClaimSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	addr, err := l.fakeSessions.StartWorker(ctx, p)
	if err != nil {
		return "", err
	}
	l.forge.mu.Lock()
	created := l.forge.clock.Add(-2 * time.Minute)
	l.forge.comments[p.IssueIID] = append(l.forge.comments[p.IssueIID], forge.IssueComment{
		ID: 8000, AuthorID: 9, CreatedAt: created,
		Body: claimLine(claimKindYes, "earlier-box", "tearlier"),
	})
	l.forge.clock = l.forge.clock.Add(time.Second)
	l.forge.mu.Unlock()
	return addr, nil
}
