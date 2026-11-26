package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/scheduler"
)

func writeActiveForCLI(t *testing.T, meta scheduler.ActiveTaskMeta) {
	t.Helper()
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scheduler.ActiveMetaPath(meta.RepoDir, meta.IssueIID), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cliActiveMeta(repoDir string, pid int, heartbeat time.Time) scheduler.ActiveTaskMeta {
	return scheduler.ActiveTaskMeta{
		Version: 1, RepoDir: repoDir, ProjectID: 1, SpecIID: 8, IssueIID: 9,
		Stage: "waiting_report", TerminalAddr: "t1", TerminalName: "repo-issue-9",
		UpdatedAt:  heartbeat,
		Supervisor: scheduler.SupervisorMeta{PID: pid, InstanceID: "test-box-1", HeartbeatAt: heartbeat},
	}
}

// dead supervisor + waiting_report：CLI 无 spec 启动时恢复原 spec。
func TestRecoverSpecFromActiveAdoptsDeadSupervisor(t *testing.T) {
	repoDir := t.TempDir()
	writeActiveForCLI(t, cliActiveMeta(repoDir, -1, time.Now()))
	var stderr bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, handled, err := recoverSpecFromActive(repoDir, &stderr, log)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("handled = true for an adoptable run")
	}
	if spec != 8 {
		t.Fatalf("spec = %d, want 8", spec)
	}
}

// live supervisor + fresh heartbeat：CLI 只报告观察方式，不进入本地调度。
func TestRecoverSpecFromActiveReportsLiveSupervisor(t *testing.T) {
	repoDir := t.TempDir()
	writeActiveForCLI(t, cliActiveMeta(repoDir, os.Getpid(), time.Now()))
	var stderr bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	spec, handled, err := recoverSpecFromActive(repoDir, &stderr, log)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || spec != 0 {
		t.Fatalf("spec/handled = %d/%v, want observation handled without a scheduler", spec, handled)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("hc attach repo-issue-9")) {
		t.Fatalf("stderr = %q, want the read-only hc attach hint", stderr.String())
	}
}

// live supervisor + stale heartbeat：宁可报错，也不能冒险双控。
func TestRecoverSpecFromActiveBlocksStaleLiveSupervisor(t *testing.T) {
	repoDir := t.TempDir()
	writeActiveForCLI(t, cliActiveMeta(repoDir, os.Getpid(), time.Now().Add(-scheduler.ActiveStaleAfter-time.Second)))
	var stderr bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, _, err := recoverSpecFromActive(repoDir, &stderr, log); err == nil {
		t.Fatal("expected a blocked stale-live supervisor error")
	}
}

// 显式输入 spec 时也必须在做任何写操作前识别 live supervisor；否则
// 即使任务级拒绝偷锁，集成分支 push/MR 准备已经违反了只读观察语义。
func TestCheckActiveSpecBeforeWritesReportsLiveSupervisor(t *testing.T) {
	repoDir := t.TempDir()
	writeActiveForCLI(t, cliActiveMeta(repoDir, os.Getpid(), time.Now()))
	err := checkActiveSpecBeforeWrites(repoDir, 8, time.Now())
	var conflict *ActiveRunConflictError
	if err == nil || !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want ActiveRunConflictError", err)
	}
	if conflict.SpecIID != 8 || len(conflict.Tasks) != 1 {
		t.Fatalf("conflict = %+v, want spec 8 with its active task", conflict)
	}
}

// 匹配 active pattern 但无法解析/版本不符的文件不能静默消失：它可能
// 代表一个仍在运行的 worker，只是当前二进制不懂新 schema。显式启动
// 必须保守阻塞并提示人工检查，而不是当成没有 active run。
func TestCheckActiveSpecBeforeWritesRejectsInvalidActiveMetadata(t *testing.T) {
	repoDir := t.TempDir()
	path := scheduler.ActiveMetaPath(repoDir, 9)
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkActiveSpecBeforeWrites(repoDir, 8, time.Now()); err == nil {
		t.Fatal("invalid active metadata did not block writes")
	}
}

// dead supervisor 的可接管现场不应阻塞显式启动；后续 scheduler 会走恢复路径。
func TestCheckActiveSpecBeforeWritesAllowsDeadSupervisor(t *testing.T) {
	repoDir := t.TempDir()
	writeActiveForCLI(t, cliActiveMeta(repoDir, -1, time.Now()))
	if err := checkActiveSpecBeforeWrites(repoDir, 8, time.Now()); err != nil {
		t.Fatalf("dead adoptable active run blocked explicit startup: %v", err)
	}
}
