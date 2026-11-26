//go:build !unix

package scheduler

import "os"

// processAlive 在非 Unix 上使用 FindProcess 的存在性语义；Windows 的
// FindProcess 会为不存在的 PID 返回错误。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}

// processStartID 在非 Unix 平台没有可移植实现。空值会让 owner 判定
// 退回 PID 检查；这不如 Linux 严格，但不会误把无法读取当成进程死亡。
func processStartID(int) string { return "" }
