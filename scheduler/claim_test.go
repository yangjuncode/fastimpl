package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// taskLockDir 把系统 temp 目录换成测试目录，使锁文件不污染真实 temp。
func taskLockDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := os.Getenv("TMPDIR")
	t.Setenv("TMPDIR", dir)
	t.Cleanup(func() { os.Setenv("TMPDIR", orig) })
	return dir
}

// TestTaskLockAliasedPathExcluded 同一仓库经符号链接别名进来仍是同一
// 把锁：第二次获取直接失败。
func TestTaskLockAliasedPathExcluded(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repoDir, alias); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if got, want := TaskLockPath(alias, 9), TaskLockPath(repoDir, 9); got != want {
		t.Fatalf("lock paths differ: %q vs %q", got, want)
	}
	first, err := AcquireTaskLock(repoDir, 9, lockContent(os.Getpid(), "box-1", "t1"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := AcquireTaskLock(alias, 9, "x"); !errors.Is(err, ErrTaskLockHeld) {
		t.Fatalf("alias acquire err = %v, want ErrTaskLockHeld", err)
	}
}

// TestTaskLockDifferentIssuesConcurrent 不同 issue 互不影响。
func TestTaskLockDifferentIssuesConcurrent(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	a, err := AcquireTaskLock(repoDir, 1, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := AcquireTaskLock(repoDir, 2, "b")
	if err != nil {
		t.Fatalf("second issue lock: %v", err)
	}
	defer b.Release()
	if a.Path() == b.Path() {
		t.Fatalf("both issues share %q", a.Path())
	}
}

// TestTaskLockReleaseAndStrangerFile 释放后可重新获取；文件被删除重建
// 时只报错，不删别人的文件。
func TestTaskLockReleaseAndStrangerFile(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	l, err := AcquireTaskLock(repoDir, 3, lockContent(os.Getpid(), testInstanceID, "t-claim"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pid=", "instance_id=", "claim="} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("lock content %q lacks %q", data, want)
		}
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file still present after release: %v", err)
	}
	again, err := AcquireTaskLock(repoDir, 3, "again")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	// 模拟人工删掉后新建：inode 变了，Release 不能删这个新文件。
	if err := os.Remove(again.Path()); err != nil {
		t.Fatal(err)
	}
	stranger := []byte("not ours\n")
	if err := os.WriteFile(again.Path(), stranger, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := again.Release(); err == nil {
		t.Fatal("Release should refuse to delete a replaced lock file")
	}
	data, err = os.ReadFile(again.Path())
	if err != nil || string(data) != string(stranger) {
		t.Fatalf("stranger file = %q, %v; want untouched", data, err)
	}
}

// TestRunTaskRefusesHeldLocalLock 本地锁已被占用（可能是残留）时只告警
// 并转人工，不偷锁、不发 claim、不启动 worker。
func TestRunTaskRefusesHeldLocalLock(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	held, err := AcquireTaskLock(repoDir, 9, lockContent(999999, "other-box", "t-old"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{}
	sessions := &fakeSessions{}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions, "tpl", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())

	err = s.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "needs attention") {
		t.Fatalf("Run err = %v, want stalled needs-attention", err)
	}
	if _, statErr := os.Stat(held.Path()); statErr != nil {
		t.Fatalf("existing lock file was removed: %v", statErr)
	}
	f.mu.Lock()
	notes := append([]string{}, f.notes...)
	claimed := append([]graph.Key{}, f.claimed...)
	f.mu.Unlock()
	if len(claimed) != 0 {
		t.Fatalf("claimed %v, want none", claimed)
	}
	for _, n := range notes {
		if strings.Contains(n, claimVersion) {
			t.Fatalf("claim marker posted despite the held lock: %v", notes)
		}
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 0 {
		t.Fatalf("workers started = %d, want 0", started)
	}
}

// TestRunTaskRespectsExistingClaim 别人的活跃认领只告警：不发新评论、
// 不关 issue、不启动 worker，也不删除别人的标记。
func TestRunTaskRespectsExistingClaim(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{authorID: 200}
	f.addComment(9, claimLine(claimKindYes, "other-box-2", "tdeadbeef"), 200)
	sessions := &fakeSessions{}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions, "tpl", repoDir, intDir, intBranch, baseName, []string{"devin"}, log)

	if err := s.Run(ctx); err == nil {
		t.Fatal("want stalled error")
	}
	f.mu.Lock()
	comments := append([]forge.IssueComment{}, f.comments[9]...)
	closed := append([]graph.Key{}, f.closed...)
	claimed := append([]graph.Key{}, f.claimed...)
	f.mu.Unlock()
	if len(comments) != 1 || comments[0].Body != claimLine(claimKindYes, "other-box-2", "tdeadbeef") {
		t.Fatalf("existing claim comments = %v, want untouched single marker", comments)
	}
	if len(closed) != 0 || len(claimed) != 0 {
		t.Fatalf("closed=%v claimed=%v, want none", closed, claimed)
	}
	out := buf.String()
	for _, want := range []string{"other-box-2", "tdeadbeef", "comment="} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q lacks %q", out, want)
		}
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 0 {
		t.Fatalf("workers started = %d, want 0", started)
	}
}

// TestRunTaskClaimNotConfirmed 抢到仲裁后指派异常（缺失/他人/关闭/回读
// 报错）都不启动 worker，并释放本次自己的标记。
func TestRunTaskClaimNotConfirmed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeForge)
	}{
		{"assignee missing", func(f *fakeForge) { f.assigneeLost = true }},
		{"assigned to other", func(f *fakeForge) { f.otherAfterClaim = map[int]bool{9: true} }},
		{"re-read fails", func(f *fakeForge) {
			f.stateErrAt = 3
			f.stateErr = errors.New("boom")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			taskLockDir(t)
			repoDir := initRepo(t)
			repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
			ctx := context.Background()
			intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
			if err != nil {
				t.Fatal(err)
			}
			f := &fakeForge{}
			tc.setup(f)
			sessions := &fakeSessions{}
			g := graph.New(graph.Key{ProjectID: 1, IID: 8})
			g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
			cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
			s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions, "tpl", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())

			if err := s.Run(ctx); err == nil {
				t.Fatal("want stalled error")
			}
			sessions.mu.Lock()
			started := len(sessions.started)
			sessions.mu.Unlock()
			if started != 0 {
				t.Fatalf("workers started = %d, want 0", started)
			}
			f.mu.Lock()
			comments := append([]forge.IssueComment{}, f.comments[9]...)
			f.mu.Unlock()
			var hasClaim, hasRelease bool
			for _, c := range comments {
				if kind, _, _, ok := parseClaim(c.Body); ok {
					switch kind {
					case claimKindYes:
						hasClaim = true
					case claimKindNo:
						hasRelease = true
					}
				}
			}
			if !hasClaim {
				t.Fatalf("no claim marker posted: %v", comments)
			}
			if !hasRelease {
				t.Fatalf("claim marker not released after the failed start: %v", comments)
			}
		})
	}
}

// TestRunTaskClosedBeforeClaim 抢到认领前 issue 已被关闭：按原语义
// 记为 closed，不启动 worker。
func TestRunTaskClosedBeforeClaim(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{closedAfterClaim: map[int]bool{9: true}}
	sessions := &fakeSessions{}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions, "tpl", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())

	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	n, _ := g.Lookup(graph.Key{ProjectID: 1, IID: 9})
	if !n.Closed {
		t.Fatalf("node = %+v, want closed", n)
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 0 {
		t.Fatalf("workers started = %d, want 0", started)
	}
}

// TestActiveClaimsArbitration 评论回放：按服务端时间与 ID 升序、release
// 只释放更早且同 author/instance/token 的认领、重复 claim 不复活。
func TestActiveClaimsArbitration(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	comments := []forge.IssueComment{
		{ID: 5, AuthorID: 1, CreatedAt: at(5), Body: claimLine(claimKindYes, "box-a", "t5")},
		{ID: 2, AuthorID: 1, CreatedAt: at(2), Body: "unrelated note"},
		{ID: 3, AuthorID: 1, CreatedAt: at(3), Body: claimLine(claimKindNo, "box-a", "t3")}, // 无对应 claim：忽略
		{ID: 1, AuthorID: 1, CreatedAt: at(1), Body: claimLine(claimKindYes, "box-b", "t1")},
		{ID: 4, AuthorID: 1, CreatedAt: at(4), Body: claimLine(claimKindNo, "box-b", "t1")},
		{ID: 6, AuthorID: 9, CreatedAt: at(6), Body: claimLine(claimKindNo, "box-a", "t5")},  // author 不同：不释放
		{ID: 7, AuthorID: 1, CreatedAt: at(5), Body: claimLine(claimKindYes, "box-a", "t5")}, // 重复 claim，同时间但 ID 更晚
	}
	active, err := activeClaims(comments)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].instance != "box-a" || active[0].token != "t5" {
		t.Fatalf("active = %+v, want only box-a/t5", active)
	}
	if active[0].commentID != 5 {
		t.Fatalf("winner comment = %d, want the first appearance 5", active[0].commentID)
	}

	// release 之后重复 claim 不能复活；重复 release 幂等。
	comments = append(comments,
		forge.IssueComment{ID: 8, AuthorID: 1, CreatedAt: at(6), Body: claimLine(claimKindNo, "box-a", "t5")},
		forge.IssueComment{ID: 9, AuthorID: 1, CreatedAt: at(7), Body: claimLine(claimKindYes, "box-a", "t5")},
		forge.IssueComment{ID: 10, AuthorID: 1, CreatedAt: at(8), Body: claimLine(claimKindNo, "box-a", "t5")},
		forge.IssueComment{ID: 11, AuthorID: 1, CreatedAt: at(9), Body: claimLine(claimKindNo, "box-a", "t5")},
	)
	active, err = activeClaims(comments)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active after release + repeated claim = %+v, want a tombstone (no revival)", active)
	}

	// 别的作者发布的同 instance/token release 不生效。
	other := []forge.IssueComment{
		{ID: 1, AuthorID: 1, CreatedAt: at(1), Body: claimLine(claimKindYes, "box-c", "t9")},
		{ID: 2, AuthorID: 2, CreatedAt: at(2), Body: claimLine(claimKindNo, "box-c", "t9")},
	}
	active, err = activeClaims(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].instance != "box-c" {
		t.Fatalf("active = %+v, want the claim from another author untouched", active)
	}
}

// TestActiveClaimsRejectsUnverifiable 缺少服务端 ID/时间/author 的标记
// 无法验证，直接报错而不是当作有效认领。
func TestActiveClaimsRejectsUnverifiable(t *testing.T) {
	for _, c := range []forge.IssueComment{
		{ID: 0, AuthorID: 1, CreatedAt: time.Now(), Body: claimLine(claimKindYes, "box-a", "t1")},
		{ID: 1, AuthorID: 0, CreatedAt: time.Now(), Body: claimLine(claimKindYes, "box-a", "t1")},
		{ID: 1, AuthorID: 1, Body: claimLine(claimKindYes, "box-a", "t1")},
	} {
		if _, err := activeClaims([]forge.IssueComment{c}); err == nil {
			t.Fatalf("comment %+v should be rejected", c)
		}
	}
}

// TestParseClaimStrict 标记必须是严格单行；多行或不认识的内容都忽略。
func TestParseClaimStrict(t *testing.T) {
	if _, _, _, ok := parseClaim(claimLine(claimKindYes, "box-a", "t1") + "\nextra"); ok {
		t.Fatal("multi-line body must not parse as a marker")
	}
	if _, _, _, ok := parseClaim("fastimpl-claim/v1 claim box a t1"); ok {
		t.Fatal("spaces inside fields must not parse")
	}
	if kind, inst, tok, ok := parseClaim("  " + claimLine(claimKindNo, "box.a-1", "t2") + "  "); !ok ||
		kind != claimKindNo || inst != "box.a-1" || tok != "t2" {
		t.Fatalf("parse = %q %q %q %v", kind, inst, tok, ok)
	}
}

// TestWatchClaimCancelsOnEarlierClaim 运行期复查：出现更早的活跃认领时
// 取消任务 context，cause 是 ErrClaimLost。
func TestWatchClaimCancelsOnEarlierClaim(t *testing.T) {
	f := &fakeForge{}
	own := f.addComment(9, claimLine(claimKindYes, testInstanceID, "mine"), 100)
	s := newTestScheduler(config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID},
		nil, f, "gitlab", graph.New(graph.Key{ProjectID: 1, IID: 8}), nil, "tpl", t.TempDir(), "", "", "", nil, slog.Default())
	s.claimRecheck = 5 * time.Millisecond

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	monitor := s.watchClaim(ctx, graph.Key{ProjectID: 1, IID: 9}, "mine", cancel)
	f.mu.Lock()
	f.comments[9] = append(f.comments[9], forge.IssueComment{
		ID: own.ID + 100, AuthorID: 1, CreatedAt: own.CreatedAt.Add(-time.Minute),
		Body: claimLine(claimKindYes, "other-box", "theirs"),
	})
	f.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	monitor.stop()
	if !errors.Is(context.Cause(ctx), ErrClaimLost) {
		t.Fatalf("cause = %v, want ErrClaimLost", context.Cause(ctx))
	}
}

// TestWatchClaimCancelsOnDeletedMarker 自己的标记被删除时同样失锁；
// 读取暂时失败不当作失锁。
func TestWatchClaimCancelsOnDeletedMarker(t *testing.T) {
	f := &fakeForge{}
	f.addComment(9, claimLine(claimKindYes, testInstanceID, "mine"), 100)
	s := newTestScheduler(config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID},
		nil, f, "gitlab", graph.New(graph.Key{ProjectID: 1, IID: 8}), nil, "tpl", t.TempDir(), "", "", "", nil, slog.Default())
	s.claimRecheck = 5 * time.Millisecond
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	monitor := s.watchClaim(ctx, graph.Key{ProjectID: 1, IID: 9}, "mine", cancel)

	// 先注入读取错误：不能因此取消。
	f.mu.Lock()
	f.commentErr = errors.New("network down")
	f.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("read error must not cancel the task: %v", context.Cause(ctx))
	}
	// 再删除自己的标记：必须取消。
	f.mu.Lock()
	f.commentErr = nil
	f.comments[9] = nil
	f.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	monitor.stop()
	if !errors.Is(context.Cause(ctx), ErrClaimLost) {
		t.Fatalf("cause = %v, want ErrClaimLost", context.Cause(ctx))
	}
}

// TestRunTaskStopsWorkerWhenEarlierClaimAppears 任务启动后出现更早的
// 活跃认领：不再继续，停掉本任务终端，不关 issue。
func TestRunTaskStopsWorkerWhenEarlierClaimAppears(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{}
	sessions := &lateClaimSessions{fakeSessions: &fakeSessions{}, forge: f}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())

	if err := s.Run(ctx); err == nil {
		t.Fatal("want stalled error after losing the claim")
	}
	sessions.mu.Lock()
	started, stopped := len(sessions.started), sessions.stopped
	sessions.mu.Unlock()
	if started != 1 {
		t.Fatalf("workers started = %d, want 1", started)
	}
	if stopped != 1 {
		t.Fatalf("workers stopped = %d, want 1 (losing the claim must stop the terminal)", stopped)
	}
	f.mu.Lock()
	closed := append([]graph.Key{}, f.closed...)
	f.mu.Unlock()
	if len(closed) != 0 {
		t.Fatalf("closed %v, want none (lost claim must not close the issue)", closed)
	}
}

// lateClaimSessions 在 worker 启动后插入一条更早的认领，模拟对手晚到
// 但服务端时间更早的标记。
type lateClaimSessions struct {
	*fakeSessions
	forge   *fakeForge
	stopped int
}

func (l *lateClaimSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	addr, err := l.fakeSessions.StartWorker(ctx, p)
	if err != nil {
		return "", err
	}
	l.forge.mu.Lock()
	own := l.forge.comments[p.IssueIID]
	created := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	if len(own) > 0 {
		created = own[0].CreatedAt.Add(-time.Minute)
	}
	l.forge.comments[p.IssueIID] = append(l.forge.comments[p.IssueIID], forge.IssueComment{
		ID: 9000, AuthorID: 7, CreatedAt: created,
		Body: claimLine(claimKindYes, "other-box", "theirs"),
	})
	l.forge.mu.Unlock()
	return addr, nil
}

func (l *lateClaimSessions) Stop(ctx context.Context, addr string, specIID int, taskName string) error {
	l.mu.Lock()
	l.stopped++
	l.mu.Unlock()
	return nil
}

// TestRunTaskTwoInstancesCompete 同账号、不同 instance、各自 checkout 绕过
// 本地锁并发抢同一 issue：只有一个启动 worker，输的一方清掉自己的标记。
func TestRunTaskTwoInstancesCompete(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	// 第一次读评论时双方会合：保证两边都在看到对方之前把 claim POST 出去，
	// 第二次读才可能同时看到两条活跃认领（否则一方可能先跑完整个任务，
	// 另一方看不到认领也跟着开 worker，测试假失败）。
	var meet sync.Mutex
	arrived := 0
	both := make(chan struct{})
	var closeOnce sync.Once
	f.claimBarrier = func(call, claims int) {
		if claims != 0 {
			return
		}
		meet.Lock()
		arrived++
		first := arrived == 1
		meet.Unlock()
		if first {
			select {
			case <-both:
			case <-time.After(5 * time.Second):
			}
			return
		}
		closeOnce.Do(func() { close(both) })
	}

	type instance struct {
		s        *Scheduler
		sessions *fakeSessions
		g        *graph.Graph
	}
	build := func(id string) instance {
		repoDir := initRepo(t)
		repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
		ctx := context.Background()
		intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
		if err != nil {
			t.Fatal(err)
		}
		sessions := &fakeSessions{}
		g := graph.New(graph.Key{ProjectID: 1, IID: 8})
		g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
		cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: id}
		// 用带令牌的模板让胜者的 agent 报告能被识别，从而走到成功收尾。
		s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
		return instance{s: s, sessions: sessions, g: g}
	}
	a, b := build("box-a"), build("box-b")
	var done sync.WaitGroup
	done.Add(2)
	for _, inst := range []instance{a, b} {
		go func(inst instance) {
			defer done.Done()
			_ = inst.s.Run(context.Background())
		}(inst)
	}
	done.Wait()

	f.mu.Lock()
	comments := append([]forge.IssueComment{}, f.comments[9]...)
	f.mu.Unlock()
	active, err := activeClaims(comments)
	if err != nil {
		t.Fatal(err)
	}
	// 两边都会在收尾时释放自己的标记（输在仲裁、赢在成功停止之后）。
	if len(active) != 0 {
		t.Fatalf("active claims = %+v, want none left after cleanup", active)
	}
	claims, releases := 0, 0
	for _, c := range comments {
		if kind, _, _, ok := parseClaim(c.Body); ok {
			if kind == claimKindYes {
				claims++
			} else {
				releases++
			}
		}
	}
	if claims != 2 || releases != 2 {
		t.Fatalf("markers claim=%d release=%d, want both sides released: %v", claims, releases, comments)
	}
	startedA, startedB := len(a.sessions.started), len(b.sessions.started)
	if startedA+startedB != 1 {
		t.Fatalf("workers started a=%d b=%d, want exactly one winner", startedA, startedB)
	}
}
