package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func CanonicalRepoPath(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func TaskLockPath(repoDir string, iid int) string {
	sum := sha256.Sum256([]byte(CanonicalRepoPath(repoDir)))
	return filepath.Join(os.TempDir(), fmt.Sprintf("fastimpl-%s-%d.lock", hex.EncodeToString(sum[:]), iid))
}

// afterLockCreate 是测试接缝：锁文件创建之后、身份校验之前调用，
// 用于模拟并发替换。
var afterLockCreate = func(string) {}

// ErrTaskLockHeld 表示同机同仓库同 issue 已有运行持有本地锁。
var ErrTaskLockHeld = errors.New("task lock is already held")

type taskLock struct {
	path string
	file *os.File
}

// AcquireTaskLock 以 O_EXCL 抢占本地任务锁并写入内容（PID、instance ID、
// claim token）。并发只有一个成功；权限/写入失败一律 fail closed，
// 不降级成"无锁运行"。
func AcquireTaskLock(repoDir string, iid int, content string) (*taskLock, error) {
	path := TaskLockPath(repoDir, iid)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrTaskLockHeld, path)
		}
		return nil, fmt.Errorf("create task lock %s: %w", path, err)
	}
	afterLockCreate(path)
	// O_EXCL 只保证创建那一刻是新的：路径可能在创建后被并发替换。用自己
	// fd 的 Stat 与 Lstat(path)（不跟随符号链接）比 SameFile，确认路径仍
	// 指向本进程打开的那个 inode；任何异常都关闭 fd 并且不删这个不确定
	// 路径，交人工确认。
	held, herr := f.Stat()
	cur, lerr := os.Lstat(path)
	if herr != nil || lerr != nil || !cur.Mode().IsRegular() || !os.SameFile(held, cur) {
		f.Close()
		return nil, fmt.Errorf("task lock %s is not the file we just created (replaced or not a regular file); "+
			"left in place: confirm no worker runs for this issue, then delete it", path)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return nil, fmt.Errorf("write task lock %s: %w (left in place; confirm no worker runs, then delete it)", path, err)
	}
	return &taskLock{path: path, file: f}, nil
}

func (l *taskLock) Path() string { return l.path }

// Update 重写仍由本锁持有的元数据内容；只用于补充终端地址等排障信息，
// 不改变锁语义。失败时调用方只告警，不影响任务。
func (l *taskLock) Update(content string) error {
	if l.file == nil {
		return errors.New("task lock is already closed")
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return err
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.WriteString(content); err != nil {
		return err
	}
	return l.file.Sync()
}

// Release 关闭自己持有的 fd 并删除锁文件，但只删自己创建的那个 inode：
// 文件已被人工删除或换成新文件时不动别人的文件，并报错说明。
func (l *taskLock) Release() error {
	if l.file == nil {
		return nil
	}
	held, err := l.file.Stat()
	l.file.Close()
	l.file = nil
	if err != nil {
		return fmt.Errorf("stat task lock %s: %w", l.path, err)
	}
	cur, err := os.Lstat(l.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat task lock %s: %w", l.path, err)
	}
	if !cur.Mode().IsRegular() || !os.SameFile(held, cur) {
		return fmt.Errorf("task lock %s was replaced; leaving the new file alone", l.path)
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove task lock %s: %w", l.path, err)
	}
	return nil
}

// Keep 关闭 fd 但保留锁文件（无法确认 worker 已停止时使用）。
func (l *taskLock) Keep() {
	if l.file == nil {
		return
	}
	l.file.Close()
	l.file = nil
}

func lockContent(pid int, instanceID, token string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid=%d\ninstance_id=%s\nclaim=%s\n", pid, instanceID, token)
	return b.String()
}

func lockContentWithTerminal(pid int, instanceID, token, addr, termName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pid=%d\ninstance_id=%s\nclaim=%s\nterminal_addr=%s\nterminal_name=%s\n", pid, instanceID, token, addr, termName)
	return b.String()
}
