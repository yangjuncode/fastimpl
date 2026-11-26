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

type promptCountSessions struct {
	*fakeSessions
	prompts int
}

func (p *promptCountSessions) Prompt(ctx context.Context, addr, text string) (uint64, error) {
	p.prompts++
	return p.fakeSessions.Prompt(ctx, addr, text)
}

// promptStageSessions 在真正发送 prompt 前读取 active metadata：发送动作
// 与 metadata 更新不可能原子完成，因此必须先进入保守的 prompting 态。
type promptStageSessions struct {
	*fakeSessions
	path   string
	stages []activeStage
}

func (p *promptStageSessions) Prompt(ctx context.Context, addr, text string) (uint64, error) {
	meta, err := readActiveMeta(p.path)
	if err != nil {
		return 0, err
	}
	p.stages = append(p.stages, meta.Stage)
	return p.fakeSessions.Prompt(ctx, addr, text)
}

// 初始 prompt 的发送窗口不能再沿用 booting：否则“Prompt 已成功但
// waiting_report 尚未落盘”的崩溃会被误判成未发送，导致重启后重发。
func TestPromptTransitionIsDurableBeforeSend(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	base := &fakeSessions{}
	s, _, repoDir := runnableTask(t, f, base, nil)
	sessions := &promptStageSessions{fakeSessions: base, path: ActiveMetaPath(repoDir, 9)}
	s.sessions = sessions
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sessions.stages) == 0 {
		t.Fatal("no prompt was sent")
	}
	if sessions.stages[0] != activeStagePrompting {
		t.Fatalf("stage at first Prompt = %q, want prompting before the send", sessions.stages[0])
	}
}

// 崩溃/关闭重启后的核心场景：旧 supervisor 已死，但 claim、worktree、
// hc 终端与 report token 都在。新 scheduler 必须直接接管原终端，从保存的
// turn baseline 等报告，随后正常集成/关闭/释放，而不是重启一个新 agent。
func TestRunTaskRecoversCrashedWaitingWorker(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{inFlight: map[int]bool{9: true}}
	base := &fakeSessions{}
	sessions := &promptCountSessions{fakeSessions: base}
	s, _, repoDir := runnableTask(t, f, sessions, nil)
	ctx := context.Background()

	// 构造崩溃前已经存在的工作区与提交：接管后不需要 agent 再写代码，
	// 只要读到原 report token 就能继续集成。
	dir, branch, err := s.repo.Create(ctx, s.branch, 9)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task9.txt"), []byte("task9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !git(dir, "add", "task9.txt") || !git(dir, "commit", "-m", "task 9") {
		t.Fatal("commit recovered worktree")
	}

	key := graph.Key{ProjectID: 1, IID: 9}
	claimToken := "claim-crashed"
	reportToken := "report-crashed"
	termName := "-issue-9" // runnableTask 未设置 repo.Name，生产环境是 <repo>-issue-<iid>
	addr := "t-crash"
	base.records = map[string]worker.Record{
		addr: {Live: true, Status: "done", Reported: true, Name: termName, PID: 4321, Cwd: repoDir, Argv: []string{"devin"}, Labels: map[string]string{
			"fastimpl": "8", "task": termName,
		}},
	}
	base.tokens = map[string]string{addr: reportToken}
	f.claimed = append(f.claimed, key)
	f.addComment(9, claimLine(claimKindYes, testInstanceID, claimToken), 100)

	startedAt := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	meta := ActiveTaskMeta{
		Version:           activeMetaVersion,
		RepoDir:           CanonicalRepoPath(repoDir),
		WorkDir:           repoDir,
		TreeDir:           s.repo.TreeDir,
		Worktree:          dir,
		IntegrationDir:    s.intDir,
		ProjectID:         1,
		SpecIID:           8,
		IssueIID:          9,
		Branch:            branch,
		IntegrationBranch: s.branch,
		BaseBranch:        s.baseName,
		TerminalAddr:      addr,
		TerminalName:      termName,
		TerminalCwd:       repoDir,
		TerminalPID:       4321,
		TerminalArgv:      []string{"devin"},
		ClaimToken:        claimToken,
		ReportToken:       reportToken,
		Stage:             activeStageWaitingReport,
		PromptBaseline:    0,
		StartedAt:         startedAt,
		Supervisor: SupervisorMeta{
			PID:         -1,
			InstanceID:  testInstanceID,
			HeartbeatAt: time.Now().Add(-time.Minute),
		},
	}
	if err := writeActiveMeta(ActiveMetaPath(repoDir, 9), meta); err != nil {
		t.Fatal(err)
	}
	// 崩溃进程来不及释放的 lock 仍在；接管方验证 supervisor 死亡后才能偷过来。
	if err := os.WriteFile(TaskLockPath(repoDir, 9), []byte(lockContent(12345, testInstanceID, claimToken)), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	base.mu.Lock()
	started := len(base.started)
	base.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want the crashed terminal reused", started)
	}
	if sessions.prompts != 0 {
		t.Fatalf("Prompt calls = %d, want the in-flight prompt continued rather than resent", sessions.prompts)
	}
	if lockExists(repoDir, 9) {
		t.Fatal("recovered task must release the stolen local lock after integration")
	}
	if _, err := os.Stat(ActiveMetaPath(repoDir, 9)); !os.IsNotExist(err) {
		t.Fatalf("active metadata still exists after successful recovery: %v", err)
	}
	claims, releases, active := markerKinds(t, f, 9)
	if claims != 1 || releases != 1 || len(active) != 0 {
		t.Fatalf("claim=%d release=%d active=%+v, want the original claim released", claims, releases, active)
	}
	f.mu.Lock()
	closed := append([]graph.Key{}, f.closed...)
	f.mu.Unlock()
	if len(closed) != 1 || closed[0] != key {
		t.Fatalf("closed = %v, want recovered issue closed", closed)
	}
	rt, ok := runtimeFor(s, key)
	if !ok || !rt.StartedAt.Equal(startedAt) {
		t.Fatalf("recovered runtime start = %v ok=%v, want original start %v", rt.StartedAt, ok, startedAt)
	}
	if rt.FinishedAt.IsZero() {
		t.Fatal("recovered runtime has no finish time")
	}
}

// heartbeat 新鲜且 supervisor 活着时，第二个 scheduler 不能偷锁/重发 prompt。
// GUI 可以把这种情况作为只读观察；调度器本身必须保守退出为 needs attention。
func TestRunTaskDoesNotStealActiveSupervisor(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := &fakeSessions{}
	s, _, repoDir := runnableTask(t, f, sessions, nil)
	meta := testActiveMeta(repoDir)
	meta.Supervisor.PID = os.Getpid()
	meta.Supervisor.HeartbeatAt = time.Now()
	if err := writeActiveMeta(ActiveMetaPath(repoDir, 9), meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TaskLockPath(repoDir, 9), []byte(lockContent(os.Getpid(), testInstanceID, meta.ClaimToken)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error when another live supervisor owns the task")
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want no steal/restart", started)
	}
	rt, _ := runtimeFor(s, graph.Key{ProjectID: 1, IID: 9})
	if rt.Attention == "" {
		t.Fatal("active supervisor conflict must surface as needs attention")
	}
	if !lockExists(repoDir, 9) {
		t.Fatal("the live owner's lock must not be removed")
	}
	if _, err := os.Stat(ActiveMetaPath(repoDir, 9)); err != nil {
		t.Fatalf("the live owner's metadata must not be removed: %v", err)
	}
	_ = os.Remove(ActiveMetaPath(repoDir, 9))
	_ = os.Remove(TaskLockPath(repoDir, 9))
	_ = forge.IssueInfo{}
}

// crashedWorkerFixture 建一份“旧 supervisor 已死、waiting/booting worker 仍可
// 接管”的现场。它与上面的端到端恢复测试共用协议，但允许每个场景调整 stage、
// 终端身份或 claim 状态。
func crashedWorkerFixture(t *testing.T, stage activeStage) (*Scheduler, *fakeForge, *fakeSessions, *promptCountSessions, string, ActiveTaskMeta) {
	t.Helper()
	taskLockDir(t)
	f := &fakeForge{inFlight: map[int]bool{9: true}}
	base := &fakeSessions{}
	sessions := &promptCountSessions{fakeSessions: base}
	s, _, repoDir := runnableTask(t, f, sessions, nil)
	ctx := context.Background()
	dir, branch, err := s.repo.Create(ctx, s.branch, 9)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task9.txt"), []byte("task9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !git(dir, "add", "task9.txt") || !git(dir, "commit", "-m", "task 9") {
		t.Fatal("commit recovered worktree")
	}
	claimToken := "claim-crashed"
	reportToken := "report-crashed"
	termName := "-issue-9"
	addr := "t-crash"
	base.records = map[string]worker.Record{
		addr: {Live: true, Status: "done", Reported: true, Name: termName, PID: 4321, Cwd: repoDir, Argv: []string{"devin"}, Labels: map[string]string{
			"fastimpl": "8", "task": termName,
		}},
	}
	base.tokens = map[string]string{addr: reportToken}
	key := graph.Key{ProjectID: 1, IID: 9}
	f.claimed = append(f.claimed, key)
	f.addComment(9, claimLine(claimKindYes, testInstanceID, claimToken), 100)
	meta := ActiveTaskMeta{
		Version: activeMetaVersion, RepoDir: CanonicalRepoPath(repoDir), WorkDir: repoDir, TreeDir: s.repo.TreeDir,
		Worktree: dir, IntegrationDir: s.intDir, ProjectID: 1, SpecIID: 8, IssueIID: 9,
		Branch: branch, IntegrationBranch: s.branch, BaseBranch: s.baseName,
		TerminalAddr: addr, TerminalName: termName, TerminalCwd: repoDir,
		TerminalPID: 4321, TerminalArgv: []string{"devin"},
		ClaimToken: claimToken, ReportToken: reportToken, Stage: stage,
		StartedAt:  time.Now().Add(-20 * time.Minute),
		Supervisor: SupervisorMeta{PID: -1, InstanceID: testInstanceID, HeartbeatAt: time.Now().Add(-time.Minute)},
	}
	if err := writeActiveMeta(ActiveMetaPath(repoDir, 9), meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TaskLockPath(repoDir, 9), []byte(lockContent(12345, testInstanceID, claimToken)), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, f, base, sessions, repoDir, meta
}

// active metadata 是接管协议的必要条件，不是可选调试信息。初始写入失败时
// 还没发 claim/启动 worker，必须 fail-closed 并释放刚拿到的本地锁，而不是
// 带着不可恢复现场继续跑。
func TestRunTaskRefusesToStartWhenActiveMetadataUnavailable(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := &fakeSessions{}
	s, _, repoDir := runnableTask(t, f, sessions, nil)
	// 注入初始 metadata 写失败。正式路径不存在，因此 recoveryCandidate 会
	// 按新任务推进；真正要验证的是任务在写不出接管 metadata 时 fail-closed。
	oldWrite := writeActiveMetaFile
	writeActiveMetaFile = func(string, []byte) error { return errors.New("injected active metadata write failure") }
	defer func() { writeActiveMetaFile = oldWrite }()
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error when active metadata cannot be written")
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want no worker without recovery metadata", started)
	}
	if lockExists(repoDir, 9) {
		t.Fatal("the local lock must be released when startup is refused before claiming")
	}
	f.mu.Lock()
	comments := len(f.comments[9])
	f.mu.Unlock()
	if comments != 0 {
		t.Fatalf("claim/comments = %d, want none before metadata is durable", comments)
	}
}

// 两个接管者不能同时进入“偷旧锁/写入新 supervisor”临界区。第一个接管者
// 即使还没来得及把 metadata 改成自己的 PID，第二个也必须被 adoption mutex
// 挡住；否则两个实例可能轮流删除对方刚创建的 lock，形成双控窗口。
func TestRecoveryAdoptionMutexSerializesLockStealing(t *testing.T) {
	taskLockDir(t)
	repoDir := t.TempDir()
	meta := testActiveMeta(repoDir)
	meta.RepoDir = CanonicalRepoPath(repoDir)
	meta.Supervisor.PID = -1
	meta.Supervisor.StartID = ""
	meta.Supervisor.HeartbeatAt = time.Now().Add(-time.Minute)
	if err := writeActiveMeta(ActiveMetaPath(repoDir, 9), meta); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TaskLockPath(repoDir, 9), []byte(lockContent(12345, testInstanceID, meta.ClaimToken)), 0o600); err != nil {
		t.Fatal(err)
	}
	lock1, releaseAdoption, err := acquireTaskLockAfterRecovery(repoDir, 9, lockContent(os.Getpid(), testInstanceID, meta.ClaimToken), true, &meta)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = lock1.Release()
		_ = os.Remove(ActiveMetaPath(repoDir, 9))
	}()
	if _, _, err := acquireTaskLockAfterRecovery(repoDir, 9, lockContent(os.Getpid()+1, testInstanceID, meta.ClaimToken), true, &meta); err == nil {
		t.Fatal("second recovery entered the adoption critical section while the first adopter held it")
	}
	if err := releaseAdoption(); err != nil {
		t.Fatal(err)
	}
}

// booting 阶段说明原 prompt 尚未被 metadata 确认发送；接管者等待 boot 后
// 发送同一份模板（同一个 report token），而不是等待一个永远不会来的报告。
func TestRunTaskRecoveryBootingSendsOriginalPromptOnce(t *testing.T) {
	s, _, base, sessions, repoDir, meta := crashedWorkerFixture(t, activeStageBooting)
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sessions.prompts != 1 {
		t.Fatalf("Prompt calls = %d, want exactly one original prompt after boot recovery", sessions.prompts)
	}
	base.mu.Lock()
	started := len(base.started)
	base.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want terminal reuse", started)
	}
	if got := base.tokens[meta.TerminalAddr]; got != meta.ReportToken {
		t.Fatalf("report token in prompt = %q, want original %q", got, meta.ReportToken)
	}
	if _, err := os.Stat(ActiveMetaPath(repoDir, 9)); !os.IsNotExist(err) {
		t.Fatalf("active metadata remains after successful boot recovery: %v", err)
	}
}

// hc 终端身份不匹配时绝不能“顺手重用”或杀掉它：那是别人的现场。任务
// 必须转 needs attention，原 lock/metadata 保持给人工排查。
func TestRunTaskRecoveryRejectsTerminalIdentityMismatch(t *testing.T) {
	s, _, base, sessions, repoDir, meta := crashedWorkerFixture(t, activeStageWaitingReport)
	key := meta.TerminalAddr
	rec := base.records[key]
	rec.Name = "someone-else"
	base.records[key] = rec
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error for terminal identity mismatch")
	}
	if sessions.prompts != 0 {
		t.Fatalf("Prompt calls = %d, want no prompt sent to a mismatched terminal", sessions.prompts)
	}
	base.mu.Lock()
	stops := append([]string(nil), base.stops...)
	started := len(base.started)
	base.mu.Unlock()
	if len(stops) != 0 || started != 0 {
		t.Fatalf("stops=%v started=%d, want the foreign terminal untouched", stops, started)
	}
	if !lockExists(repoDir, 9) {
		t.Fatal("identity mismatch must keep the original lock for manual cleanup")
	}
	if _, err := os.Stat(ActiveMetaPath(repoDir, 9)); err != nil {
		t.Fatalf("identity mismatch must keep active metadata: %v", err)
	}
	rt, _ := runtimeFor(s, graph.Key{ProjectID: 1, IID: 9})
	if rt.Attention == "" {
		t.Fatal("identity mismatch must surface as needs attention")
	}
	_ = os.Remove(ActiveMetaPath(repoDir, 9))
	_ = os.Remove(TaskLockPath(repoDir, 9))
}

// metadata 里的 argv/pid 缺失时不能默认相信终端。缺这些字段说明接管
// 边界不完整，必须保守阻塞，而不是把同名 shell 当成原 agent。
func TestRunTaskRecoveryRejectsMissingTerminalProcessIdentity(t *testing.T) {
	s, _, base, sessions, repoDir, _ := crashedWorkerFixture(t, activeStageWaitingReport)
	meta := testActiveMeta(repoDir)
	// crashedWorkerFixture 已写一份完整 metadata；这里读取后删掉 argv/pid
	// 再覆盖，模拟 schema 内部字段不完整或手工损坏的现场。
	path := ActiveMetaPath(repoDir, 9)
	current, err := readActiveMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	current.TerminalArgv = nil
	current.TerminalPID = 0
	if err := writeActiveMeta(path, current); err != nil {
		t.Fatal(err)
	}
	_ = meta
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error for incomplete terminal process identity")
	}
	if sessions.prompts != 0 {
		t.Fatalf("Prompt calls = %d, want no prompt sent to an unverified terminal", sessions.prompts)
	}
	base.mu.Lock()
	started := len(base.started)
	base.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want no replacement worker", started)
	}
	_ = os.Remove(path)
	_ = os.Remove(TaskLockPath(repoDir, 9))
}

// base 冲突集成终端也必须有可核验的 cwd/pid/argv。只有 name/labels/live
// 不足以证明它是原集成 agent；缺进程身份时宁可阻塞，也不能把错误终端
// 当成正在解决冲突的 agent 继续等。
func TestRunTaskRecoveryRejectsIntegrationTerminalMissingProcessIdentity(t *testing.T) {
	s, _, base, sessions, repoDir, _ := crashedWorkerFixture(t, activeStageIntegrating)
	path := ActiveMetaPath(repoDir, 9)
	current, err := readActiveMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	intName := "repo-issue-8-integration"
	intAddr := "t-integration"
	current.IntegrationPhase = integrationPhaseResolvingBaseConflict
	current.IntegrationTerminalAddr = intAddr
	current.IntegrationTerminalName = intName
	current.IntegrationToken = "integration-token"
	current.IntegrationPromptSent = true
	if err := writeActiveMeta(path, current); err != nil {
		t.Fatal(err)
	}
	base.records[intAddr] = worker.Record{Live: true, Status: "done", Reported: true, Name: intName, PID: 5432, Cwd: current.IntegrationDir, Argv: []string{"devin"}, Labels: map[string]string{
		"fastimpl": "8", "task": intName,
	}}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error for incomplete integration terminal process identity")
	}
	if sessions.prompts != 0 {
		t.Fatalf("Prompt calls = %d, want no prompt sent to an unverified integration terminal", sessions.prompts)
	}
	base.mu.Lock()
	started := len(base.started)
	base.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want no replacement integration worker", started)
	}
	_ = os.Remove(path)
	_ = os.Remove(TaskLockPath(repoDir, 9))
}

// integrating 阶段的简单任务 merge/push 是幂等的：崩溃重启后可以从原
// report token 再进入 tryMerge；若上次 merge 已完成，git merge 只是
// already-up-to-date，不会重复生成任务提交或重发 prompt。
func TestRunTaskRecoversIntegratingMerge(t *testing.T) {
	s, _, base, sessions, repoDir, _ := crashedWorkerFixture(t, activeStageIntegrating)
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sessions.prompts != 0 {
		t.Fatalf("Prompt calls = %d, want integration recovery without resending the task prompt", sessions.prompts)
	}
	base.mu.Lock()
	started := len(base.started)
	base.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want terminal reuse", started)
	}
	if !lockExists(repoDir, 9) {
		// 成功时 lock 应已释放；这个断言只是让路径名参与测试，真正要求见下。
	} else {
		t.Fatal("successful integrating recovery must release the local lock")
	}
	if _, err := os.Stat(ActiveMetaPath(repoDir, 9)); !os.IsNotExist(err) {
		t.Fatalf("active metadata remains after integrating recovery: %v", err)
	}
}

// base 冲突的集成 agent 也能无缝续等：metadata 中有原 token/baseline 且
// prompt_sent=true 时，resolveBaseConflict 只接管原 hc 终端并等待报告，
// 不 StartWorker、不重发冲突 prompt。
func TestResolveBaseConflictRecoveryContinuesOriginalIntegrationPrompt(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	base := &fakeSessions{}
	sessions := &promptCountSessions{fakeSessions: base}
	cfg := config.Config{Parallelism: 1, SpecIID: 8, Retries: 2, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, &fakeForge{}, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	termName := s.intTermName()
	addr := "t-integration-crash"
	token := "integration-report-token"
	base.records = map[string]worker.Record{
		addr: {Live: true, Status: "done", Reported: true, Name: termName, PID: 5432, Cwd: intDir, Argv: []string{"devin"}, Labels: map[string]string{
			"fastimpl": "8", "task": termName,
		}},
	}
	base.tokens = map[string]string{addr: token}
	meta := ActiveTaskMeta{
		IntegrationPhase:        integrationPhaseResolvingBaseConflict,
		IntegrationTerminalAddr: addr,
		IntegrationTerminalName: termName,
		IntegrationTerminalCwd:  intDir,
		IntegrationTerminalPID:  5432,
		IntegrationTerminalArgv: []string{"devin"},
		IntegrationToken:        token,
		IntegrationBaseline:     0,
		IntegrationPromptSent:   true,
	}
	if err := s.resolveBaseConflict(ctx, []string{"base.txt"}, nil, &meta); err != nil {
		t.Fatal(err)
	}
	if sessions.prompts != 0 {
		t.Fatalf("Prompt calls = %d, want the recovered integration prompt continued rather than resent", sessions.prompts)
	}
	base.mu.Lock()
	started := len(base.started)
	base.mu.Unlock()
	if started != 0 {
		t.Fatalf("StartWorker calls = %d, want the original integration terminal reused", started)
	}
	if s.intAddr != addr {
		t.Fatalf("intAddr = %q, want recovered %q", s.intAddr, addr)
	}
}

// 即使 adoption flock 已串行化，也不能盲删同路径文件。lock 内容必须仍
// 指向 metadata 里那个已死 supervisor/claim；否则它可能是人工或其他版本
// 替换过的现场，删除它会毁掉唯一的人工排查线索。
func TestRecoveryAdoptionRejectsMismatchedLockContent(t *testing.T) {
	taskLockDir(t)
	repoDir := t.TempDir()
	meta := testActiveMeta(repoDir)
	meta.RepoDir = CanonicalRepoPath(repoDir)
	meta.Supervisor.PID = -1
	meta.Supervisor.StartID = ""
	meta.Supervisor.HeartbeatAt = time.Now().Add(-time.Minute)
	if err := writeActiveMeta(ActiveMetaPath(repoDir, 9), meta); err != nil {
		t.Fatal(err)
	}
	lockPath := TaskLockPath(repoDir, 9)
	stranger := lockContent(9999, testInstanceID, "stranger-claim")
	if err := os.WriteFile(lockPath, []byte(stranger), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acquireTaskLockAfterRecovery(repoDir, 9, lockContent(os.Getpid(), testInstanceID, meta.ClaimToken), true, &meta); err == nil {
		t.Fatal("adoption stole a lock whose claim does not match the active metadata")
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stranger {
		t.Fatalf("mismatched lock content changed to %q, want untouched", data)
	}
	_ = os.Remove(ActiveMetaPath(repoDir, 9))
	_ = os.Remove(lockPath)
}

type bootCountSessions struct {
	*fakeSessions
	boots int
}

func (b *bootCountSessions) AwaitBoot(ctx context.Context, addr string) (worker.Record, error) {
	b.boots++
	return b.fakeSessions.AwaitBoot(ctx, addr)
}

// booting 是恢复协议的一部分：如果连这个阶段都写不进去，就不能继续
// AwaitBoot/prompt。否则崩溃恢复方会因为 metadata 仍停在 claimed 而保守阻塞，
// 明明还没发送 prompt 的现场也失去本可安全的无缝接管能力。
func TestBootingTransitionFailureStopsBeforeAwaitBoot(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	base := &fakeSessions{}
	sessions := &bootCountSessions{fakeSessions: base}
	s, _, _ := runnableTask(t, f, sessions, nil)
	oldWrite := writeActiveMetaFile
	writeActiveMetaFile = func(path string, data []byte) error {
		if strings.Contains(string(data), `"stage": "booting"`) {
			return errors.New("injected booting metadata write failure")
		}
		return oldWrite(path, data)
	}
	defer func() { writeActiveMetaFile = oldWrite }()
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want the task to need attention rather than continue without durable booting metadata")
	}
	if sessions.boots != 0 {
		t.Fatalf("AwaitBoot calls = %d, want no boot/prompt after booting metadata failure", sessions.boots)
	}
}
