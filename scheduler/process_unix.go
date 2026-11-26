//go:build unix

package scheduler

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// processAlive 用 signal 0 检查本机 PID。EPERM 也说明进程存在，只是
// 不属于当前用户；这足够防止把活跃 supervisor 当成死亡进程接管。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processStartID 返回 Linux /proc 里的进程启动 tick。comm 字段可能含
// 空格或括号，因此从最后一个右括号之后按 proc(5) 的字段位置解析：
// 剩余字段第 20 个（index 19）就是整体第 22 个字段 starttime。
func processStartID(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	if end < 0 || end+2 >= len(text) {
		return ""
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}
