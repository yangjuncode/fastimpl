// Package files 实现 fastimpl 本地文件的统一查找约定：
// 命令行显式路径 > <work-dir>/<name> > <exe 同目录>/<name>。
// 用于配置（fastimpl.toml）、提示词模板（fastimpl.prompt.md）等文件。
package files

import (
	"fmt"
	"os"
	"path/filepath"
)

// Resolve 返回本地文件的实际路径：
//  1. explicit 非空（命令行显式指定）→ 原样返回；
//  2. <workDir>/<name> 存在 → 返回它；
//  3. 否则返回 <exeDir>/<name>。
func Resolve(workDir, exeDir, name, explicit string) string {
	if explicit != "" {
		return explicit
	}
	local := filepath.Join(workDir, name)
	if _, err := os.Stat(local); err == nil {
		return local
	}
	return filepath.Join(exeDir, name)
}

// ExeDir 返回当前可执行文件所在目录；符号链接先解析，
// 使经 symlink 安装的 fastimpl 仍能找到旁边的文件。
func ExeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}
