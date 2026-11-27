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

// WriteFileAtomic 用同目录临时文件 + rename 原子替换 path。已有文件
// 保留原权限；新文件使用 perm。热加载读者因此要么看到旧完整文件，
// 要么看到新完整文件，不会读到 GUI 保存过程中的半份 TOML/模板。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
