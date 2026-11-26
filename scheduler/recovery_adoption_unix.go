//go:build unix

package scheduler

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockRecoveryAdoption 用 flock 串行化同一 issue 的接管临界区。锁附着
// 在打开的 fd 上：进程崩溃或退出会自动释放；文件本身可以长期留在 temp
// 目录，不会像 O_EXCL 文件那样变成 stale mutex。
func lockRecoveryAdoption(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open active recovery adoption lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another fastimpl instance is adopting this active worker: %w", err)
	}
	return f.Close, nil
}
