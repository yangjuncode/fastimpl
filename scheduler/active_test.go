package scheduler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testActiveMeta(repoDir string) ActiveTaskMeta {
	return ActiveTaskMeta{
		Version:           activeMetaVersion,
		RepoDir:           CanonicalRepoPath(repoDir),
		WorkDir:           "/work",
		TreeDir:           "/work/repo/.worktrees",
		Worktree:          "/work/repo/.worktrees/issue-9",
		IntegrationDir:    "/work/repo/.worktrees/issue-8-integration",
		ProjectID:         1,
		SpecIID:           8,
		IssueIID:          9,
		Branch:            "issue-9",
		IntegrationBranch: "issue-8-integration",
		BaseBranch:        "main",
		TerminalAddr:      "t9",
		TerminalName:      "repo-issue-9",
		TerminalCwd:       "/work",
		TerminalPID:       2345,
		TerminalArgv:      []string{"devin", "--permission-mode", "dangerous"},
		HCBinary:          "hc",
		ClaimToken:        "claim-token",
		ReportToken:       "report-token",
		Stage:             activeStageWaitingReport,
		PromptBaseline:    3,
		Attempts:          1,
		StartedAt:         time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		Supervisor: SupervisorMeta{
			PID:         os.Getpid(),
			InstanceID:  testInstanceID,
			Executable:  "fastimpl-gui",
			Hostname:    "box",
			HeartbeatAt: time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC),
		},
	}
}

// metadata 必须完整往返：重启后的接管与 GUI 扫描都靠这些字段判断。
func TestActiveMetaRoundTripAndScan(t *testing.T) {
	repoDir := t.TempDir()
	meta := testActiveMeta(repoDir)
	path := ActiveMetaPath(repoDir, 9)
	if !strings.HasSuffix(path, ".active.json") {
		t.Fatalf("ActiveMetaPath = %q, want an active.json sidecar", path)
	}
	if err := writeActiveMeta(path, meta); err != nil {
		t.Fatal(err)
	}
	got, err := readActiveMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Worktree != meta.Worktree || got.ReportToken != meta.ReportToken || got.PromptBaseline != 3 || got.TerminalPID != 2345 {
		t.Fatalf("round trip lost recovery fields: %+v", got)
	}
	metas, err := ScanActiveTasks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].SpecIID != 8 || metas[0].IssueIID != 9 {
		t.Fatalf("ScanActiveTasks = %+v, want the written task", metas)
	}
	// 同一仓库其他 issue 也能扫描出来，供 GUI 未输入 spec 时发现运行。
	meta.IssueIID = 10
	meta.SpecIID = 12
	if err := writeActiveMeta(ActiveMetaPath(repoDir, 10), meta); err != nil {
		t.Fatal(err)
	}
	metas, err = ScanActiveTasks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("ScanActiveTasks len = %d, want 2", len(metas))
	}
}

// 共享 temp 目录里的 metadata 写入不能使用固定 tmp 路径。预先放一个
// 符号链接时必须被完全绕开：既不能写到链接目标，也不能泄露 token。
func TestWriteActiveMetaIgnoresFixedTmpSymlink(t *testing.T) {
	repoDir := t.TempDir()
	meta := testActiveMeta(repoDir)
	path := ActiveMetaPath(repoDir, 9)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".tmp"); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	defer os.Remove(path + ".tmp")
	if err := writeActiveMeta(path, meta); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "do not overwrite" {
		t.Fatalf("fixed tmp symlink target changed to %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("active metadata mode = %o, want 0600", info.Mode().Perm())
	}
}

// heartbeat 新鲜且 supervisor 活着：另一个实例只能只读观察；进程已死时
// 即使 heartbeat 刚写过也可以接管，因为已经不存在会双控的监督者。
func TestActiveOwnerState(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	meta := testActiveMeta(t.TempDir())
	meta.Supervisor.PID = os.Getpid()
	meta.Supervisor.HeartbeatAt = now.Add(-time.Minute)
	if got := meta.OwnerState(now); got != OwnerActive {
		t.Fatalf("OwnerState fresh+alive = %v, want active", got)
	}
	meta.Supervisor.PID = -1
	meta.Supervisor.HeartbeatAt = now.Add(-time.Second)
	if got := meta.OwnerState(now); got != OwnerAdoptable {
		t.Fatalf("OwnerState fresh+dead = %v, want adoptable", got)
	}
	meta.Supervisor.PID = os.Getpid()
	meta.Supervisor.HeartbeatAt = now.Add(-ActiveStaleAfter - time.Second)
	if got := meta.OwnerState(now); got != OwnerBlocked {
		t.Fatalf("OwnerState stale+alive = %v, want blocked", got)
	}
	meta.Supervisor.PID = -1
	if got := meta.OwnerState(now); got != OwnerAdoptable {
		t.Fatalf("OwnerState stale+dead = %v, want adoptable", got)
	}
}

// 半写的 JSON 不应让兼容扫描失败；但 detailed 扫描必须把它单独报出来，
// 供会影响 worker 的启动决策保守阻塞。
func TestScanActiveTasksSkipsInvalidFiles(t *testing.T) {
	repoDir := t.TempDir()
	path := ActiveMetaPath(repoDir, 9)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	metas, err := ScanActiveTasks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("ScanActiveTasks = %+v, want invalid metadata skipped", metas)
	}
	_, invalid, err := ScanActiveTasksDetailed(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) != 1 || invalid[0] != path {
		t.Fatalf("invalid scan paths = %v, want %s", invalid, path)
	}
}

// deadPIDForTest 启动并结束一个短生命周期进程，返回一个已经被系统回收的 PID。
// 用它验证死亡 supervisor 判定，而不是猜一个“大概不存在”的 PID。
func deadPIDForTest(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run a short-lived process: %v", err)
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if processAlive(pid) {
		t.Skipf("pid %d is still alive after the helper exited", pid)
	}
	return pid
}

// RecoveryState 把 owner/heartbeat 与任务阶段合成一个启动决策：
// 活 supervisor 只观察；死 supervisor 且仍在 boot/wait/可幂等重入的
// integrating 阶段才自动接管；心跳卡住或 needs-attention 等不明阶段都保守阻塞。
func TestActiveRecoveryState(t *testing.T) {
	now := time.Now()
	base := ActiveTaskMeta{
		Version: activeMetaVersion,
		Stage:   activeStageWaitingReport,
		Supervisor: SupervisorMeta{
			PID: os.Getpid(), StartID: processStartID(os.Getpid()), HeartbeatAt: now,
		},
	}
	if got := base.RecoveryState(now); got != ActiveRecoveryObserve {
		t.Fatalf("live fresh owner state = %v, want observe", got)
	}
	stale := base
	stale.Supervisor.HeartbeatAt = now.Add(-ActiveStaleAfter - time.Second)
	if got := stale.RecoveryState(now); got != ActiveRecoveryBlocked {
		t.Fatalf("live stale owner state = %v, want blocked", got)
	}
	dead := base
	dead.Supervisor.PID = deadPIDForTest(t)
	if got := dead.RecoveryState(now); got != ActiveRecoveryAdoptable {
		t.Fatalf("dead waiting owner state = %v, want adoptable", got)
	}
	integrating := dead
	integrating.Stage = activeStageIntegrating
	if got := integrating.RecoveryState(now); got != ActiveRecoveryAdoptable {
		t.Fatalf("dead integrating owner state = %v, want adoptable because merge/push recovery is idempotent", got)
	}
	attention := dead
	attention.Stage = activeStageNeedsAttention
	if got := attention.RecoveryState(now); got != ActiveRecoveryBlocked {
		t.Fatalf("dead needs-attention owner state = %v, want blocked", got)
	}
}

// ChooseActiveSpec 只在没有活跃 supervisor 时才返回可接管的 spec；
// 有活跃 supervisor 的 spec 优先按 observe 返回，卡住/不明按 blocked 返回。
func TestChooseActiveSpec(t *testing.T) {
	now := time.Now()
	live := SupervisorMeta{PID: os.Getpid(), StartID: processStartID(os.Getpid()), HeartbeatAt: now}
	deadPID := deadPIDForTest(t)
	metas := []ActiveTaskMeta{
		{SpecIID: 8, IssueIID: 9, Stage: activeStageWaitingReport, Supervisor: live, UpdatedAt: now.Add(-time.Minute)},
		{SpecIID: 7, IssueIID: 3, Stage: activeStageWaitingReport, Supervisor: SupervisorMeta{PID: deadPID, HeartbeatAt: now}, UpdatedAt: now},
	}
	choice := ChooseActiveSpec(metas, now)
	if choice.SpecIID != 8 || choice.State != ActiveRecoveryObserve {
		t.Fatalf("choice = %+v, want the live supervisor's spec observed rather than silently adopting another spec", choice)
	}

	deadOnly := metas[1:]
	choice = ChooseActiveSpec(deadOnly, now)
	if choice.SpecIID != 7 || choice.State != ActiveRecoveryAdoptable {
		t.Fatalf("choice = %+v, want spec 7 adoptable", choice)
	}
	if got := choice.Tasks; len(got) != 1 || got[0].IssueIID != 3 {
		t.Fatalf("choice tasks = %+v, want only spec 7 metadata", got)
	}
}

// supervisor identity 不能只靠 PID：StartID 被改写后，即使 PID 仍指向一个
// 活进程，也不能把它当成原 supervisor。StartID 为空是旧格式，退回 PID。
func TestSupervisorIdentityGuardsPIDReuse(t *testing.T) {
	sup := currentSupervisor(testInstanceID)
	if !supervisorAlive(sup) {
		t.Fatalf("current supervisor %+v must be considered alive", sup)
	}
	if processStartID(os.Getpid()) != "" && sup.StartID == "" {
		t.Fatal("currentSupervisor did not record the process start identity on a /proc system")
	}
	mutated := sup
	mutated.StartID = "definitely-not-the-original-start-tick"
	if processStartID(os.Getpid()) != "" && supervisorAlive(mutated) {
		t.Fatal("a reused PID with a different StartID must not count as the original supervisor")
	}
	legacy := sup
	legacy.StartID = ""
	if !supervisorAlive(legacy) {
		t.Fatal("legacy metadata without StartID should fall back to the PID check")
	}
}

// heartbeat goroutine 必须按可配置间隔持续刷新同一个原子 metadata 文件；
// cancel 后退出，不留后台 ticker。
func TestActiveHeartbeatRefreshesMetadata(t *testing.T) {
	repoDir := t.TempDir()
	path := ActiveMetaPath(repoDir, 9)
	meta := testActiveMeta(repoDir)
	meta.Supervisor.PID = os.Getpid()
	meta.Supervisor.HeartbeatAt = time.Now().Add(-time.Hour)
	w, err := newActiveWriter(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := readActiveMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &Scheduler{activeHeartbeat: 5 * time.Millisecond, log: log}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startActiveHeartbeat(ctx, w)
	deadline := time.Now().Add(2 * time.Second)
	var got ActiveTaskMeta
	for time.Now().Before(deadline) {
		got, err = readActiveMeta(path)
		if err == nil && got.Supervisor.HeartbeatAt.After(initial.Supervisor.HeartbeatAt) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !got.Supervisor.HeartbeatAt.After(initial.Supervisor.HeartbeatAt) {
		t.Fatalf("heartbeat did not refresh: initial %v got %v", initial.Supervisor.HeartbeatAt, got.Supervisor.HeartbeatAt)
	}
	cancel()
	_ = w.remove()
}

// metadata 删除是终态：停止 heartbeat 时可能已经有一个 tick 正在写入；
// 这个迟到的写不得在 remove 后把文件重新创建出来，否则已完成任务会被
// 下次启动误判成崩溃现场。
func TestActiveWriterDoesNotRecreateMetadataAfterRemove(t *testing.T) {
	repoDir := t.TempDir()
	meta := testActiveMeta(repoDir)
	path := ActiveMetaPath(repoDir, 9)
	w, err := newActiveWriter(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.remove(); err != nil {
		t.Fatal(err)
	}
	if err := w.heartbeat(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("heartbeat recreated removed active metadata: %v", err)
	}
}
