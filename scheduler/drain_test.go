package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
)

// TestTaskLockReplacedPathRejected 创建后被并发替换成另一个普通文件时：
// 身份校验失败、返回错误、且不删那个不确定的路径。
func TestTaskLockReplacedPathRejected(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	var replaced string
	orig := afterLockCreate
	afterLockCreate = func(path string) {
		// 把刚创建的锁换成另一个普通文件（新 inode）。
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove fresh lock: %v", err)
		}
		if err := os.WriteFile(path, []byte("stranger\n"), 0o600); err != nil {
			t.Fatalf("write stranger lock: %v", err)
		}
		replaced = path
	}
	t.Cleanup(func() { afterLockCreate = orig })

	lock, err := AcquireTaskLock(repoDir, 9, lockContent(os.Getpid(), "box-1", "t1"))
	if err == nil {
		t.Fatal("want an error when the lock path was replaced between create and verify")
	}
	if lock != nil {
		t.Fatal("no lock may be returned for an unverified path")
	}
	if !strings.Contains(err.Error(), "not the file we just created") {
		t.Fatalf("error = %v, want it to explain the identity mismatch", err)
	}
	if replaced == "" {
		t.Fatal("seam did not run")
	}
	data, rerr := os.ReadFile(replaced)
	if rerr != nil || string(data) != "stranger\n" {
		t.Fatalf("replaced file = %q, %v; want it left alone", data, rerr)
	}
}

// TestTaskLockSymlinkRejectedNextTime 上次残留被替换成符号链接：不把链接
// 当 owned lock，也不跟随链接删除目标。
func TestTaskLockSymlinkRejected(t *testing.T) {
	taskLockDir(t)
	repoDir := initRepo(t)
	path := TaskLockPath(repoDir, 9)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := AcquireTaskLock(repoDir, 9, "x"); !errors.Is(err, ErrTaskLockHeld) {
		t.Fatalf("err = %v, want ErrTaskLockHeld for the existing symlink", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "keep\n" {
		t.Fatalf("symlink target = %q, %v; want untouched", data, err)
	}
}

// TestRunDrainsBeforeReturning 取消时 Run 必须先等已派任务的清理
// （停终端 → 释放认领 → 删本地锁）做完再返回。
func TestRunDrainsBeforeReturning(t *testing.T) {
	taskLockDir(t)
	f := &fakeForge{}
	sessions := newBlockingSessions(2)
	s, _, repoDir := runnableTask(t, f, sessions, nil)
	// 第二个就绪任务，验证 len(running) > 1 的 drain。
	s.g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 10}, Title: "task B", Kind: graph.KindChild})
	s.cfg.Parallelism = 2

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// 两个 worker 都启动并卡在 AwaitBoot 之后。
	sessions.waitStarted(t)
	cleanupSaw := make(chan struct{})
	sessions.onStop = func() {
		// 每个任务清完自己的锁/claim 后仍可能等另一个任务 drain；这里只
		// 记录 Stop 已发生，真正的“清理必须先于 Run 返回”在下方统一断言。
		select {
		case <-cleanupSaw:
		default:
			close(cleanupSaw)
		}
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return: drain waits for dispatched tasks")
	}
	// 返回后清理必须已完成：worker 终端已停、认领已释放、本地锁已删。
	sessions.mu.Lock()
	stops := len(sessions.stops)
	sessions.mu.Unlock()
	if stops != 2 {
		t.Fatalf("stops = %d, want 2 (both dispatched tasks stopped before Run returned)", stops)
	}
	select {
	case <-cleanupSaw:
	default:
		t.Fatal("no stop was observed")
	}
	for _, iid := range []int{9, 10} {
		if lockExists(repoDir, iid) {
			t.Fatalf("local lock for %d still exists after Run returned", iid)
		}
		f.mu.Lock()
		active, aerr := activeClaims(f.comments[iid])
		f.mu.Unlock()
		if aerr != nil {
			t.Fatal(aerr)
		}
		if len(active) != 0 {
			t.Fatalf("issue %d still has an active claim after Run returned: %+v", iid, active)
		}
	}
}

// blockingSessions 让 worker 停在 AwaitBoot，直到 ctx 被取消；Stop 走
// 真实记录并触发 onStop 回调。
type blockingSessions struct {
	*fakeSessions
	want    int
	ready   chan struct{}
	closeOn sync.Once
	onStop  func()
}

func newBlockingSessions(want int) *blockingSessions {
	return &blockingSessions{fakeSessions: &fakeSessions{}, want: want, ready: make(chan struct{})}
}

func (b *blockingSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	addr, err := b.fakeSessions.StartWorker(ctx, p)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	started := len(b.started)
	b.mu.Unlock()
	if started >= b.want {
		b.closeOn.Do(func() { close(b.ready) })
	}
	return addr, nil
}

func (b *blockingSessions) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-b.ready:
	case <-time.After(10 * time.Second):
		b.mu.Lock()
		got := len(b.started)
		b.mu.Unlock()
		t.Fatalf("only %d of %d workers started", got, b.want)
	}
}

func (b *blockingSessions) AwaitBoot(ctx context.Context, addr string) (worker.Record, error) {
	select {
	case <-ctx.Done():
		return worker.Record{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return worker.Record{}, errors.New("boot timeout in test")
	}
}

func (b *blockingSessions) Stop(ctx context.Context, addr string, specIID int, taskName string) error {
	if b.onStop != nil {
		b.onStop()
	}
	return b.fakeSessions.Stop(ctx, addr, specIID, taskName)
}
