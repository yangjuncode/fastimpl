package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// lockHelperEnv 让测试进程以"持锁子进程"身份运行。
const lockHelperEnv = "FASTIMPL_TEST_LOCK_HELPER"

// TestMain 支持以子进程身份持锁：父测试用同一测试二进制起一个真正独立
// 的进程，验证 O_EXCL 跨进程互斥（同进程 goroutine 的测试覆盖不到这点）。
func TestMain(m *testing.M) {
	if repo := os.Getenv(lockHelperEnv); repo != "" {
		lock, err := AcquireTaskLock(repo, 9, lockContent(os.Getpid(), "helper-box", "thelp"))
		if err != nil {
			os.Stderr.WriteString("helper: " + err.Error() + "\n")
			os.Exit(2)
		}
		os.Stdout.WriteString("HELD " + lock.Path() + "\n")
		// 等父进程读完 stdout 再退出（父进程 kill 或关闭 stdin 时结束）。
		_, _ = os.Stdin.Read(make([]byte, 1))
		lock.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestTaskLockCrossProcess 真正的独立子进程持有锁时，本进程拿不到。
func TestTaskLockCrossProcess(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)

	cmd := exec.Command(os.Args[0], "-test.run=TestTaskLockCrossProcessHelper")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+repoDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()
	buf := make([]byte, 512)
	n, err := stdout.Read(buf)
	if err != nil {
		t.Fatalf("read helper output: %v", err)
	}
	if !strings.HasPrefix(string(buf[:n]), "HELD ") {
		t.Fatalf("helper output = %q, want HELD <path>", buf[:n])
	}
	held := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(buf[:n])), "HELD "))
	if held != TaskLockPath(repoDir, 9) {
		t.Fatalf("helper holds %q, want %q", held, TaskLockPath(repoDir, 9))
	}
	if _, err := AcquireTaskLock(repoDir, 9, "parent"); !errors.Is(err, ErrTaskLockHeld) {
		t.Fatalf("parent acquire err = %v, want ErrTaskLockHeld while the child holds it", err)
	}
}

// TestTaskLockCrossProcessHelper 只是为了让 -run 能选中父测试里的子进程
// 分支：真正的逻辑在 TestMain 的 helper 分支里。
func TestTaskLockCrossProcessHelper(t *testing.T) {
	t.Skip("run through TestMain's helper branch")
}

// TestRunTaskStopFailureKeepsLocks 失锁后停终端失败：两把锁都保留，
// 不退化成"已释放"。
func TestRunTaskStopFailureKeepsLocks(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := &fakeSessions{stopErr: errors.New("hc stop timeout")}
	s, _, repoDir := runnableTask(t, f, &loseClaimSessions{fakeSessions: sessions, forge: f}, nil)

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("want stalled error")
	}
	if !lockExists(repoDir, 9) {
		t.Fatal("local lock must be kept when the stop fails")
	}
	_, releases, active := markerKinds(t, f, 9)
	if releases != 0 {
		t.Fatalf("release markers = %d, want the claim kept when the worker may still run", releases)
	}
	ours := false
	for _, a := range active {
		if a.instance == testInstanceID {
			ours = true
		}
	}
	if !ours {
		t.Fatalf("active = %+v, want our claim kept", active)
	}
}

// TestRunTaskBoundedOnLostClaim 竞争失败路径必须在有界时间内结束。
func TestRunTaskBoundedOnLostClaim(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := &fakeSessions{}
	s, _, _ := runnableTask(t, f, &loseClaimSessions{fakeSessions: sessions, forge: f}, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Run(ctx); err == nil {
		t.Fatal("want stalled error")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("lost-claim path took %v, want a bounded stop", elapsed)
	}
}
