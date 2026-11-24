package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// InstanceIDKey 是机器标识在 fastimpl.toml 中的键名。
const InstanceIDKey = "instance_id"

// maxInstanceIDLen 限制显式实例 ID 的长度，避免评论标记被刷爆。
const maxInstanceIDLen = 128

// instanceFile / instanceHostname 是可测试的接缝：前者给出机器 ID
// 所在的配置文件（exe 同目录），后者给出主机名。
var (
	instanceFile = func() (string, error) {
		dir, err := exeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, DefaultFile), nil
	}
	instanceHostname = os.Hostname
)

func ValidInstanceID(id string) bool { return validInstanceID(id) }

func validInstanceID(id string) bool {
	if id == "" || len(id) > maxInstanceIDLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func sanitizeHost(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "host"
	}
	return out
}

// newInstanceID 生成 "<机器名>-<随机哈希>" 形式的实例 ID；超长主机名
// 会被截断，保证结果不超过 maxInstanceIDLen。
func newInstanceID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	host, err := instanceHostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "host"
	}
	suffix := "-" + hex.EncodeToString(buf[:])
	host = sanitizeHost(host)
	if max := maxInstanceIDLen - len(suffix); len(host) > max {
		host = strings.TrimRight(host[:max], "-.")
	}
	return host + suffix, nil
}

func readInstanceID(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read config %s: %w", path, err)
	}
	var raw rawConfig
	if err := toml.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("decode config %s: %w", path, err)
	}
	if raw.InstanceID == nil {
		return "", nil
	}
	id := strings.TrimSpace(*raw.InstanceID)
	if !validInstanceID(id) {
		return "", fmt.Errorf("invalid instance_id %q in %s: use only [A-Za-z0-9._-], at most %d chars",
			id, path, maxInstanceIDLen)
	}
	return id, nil
}

func withInstanceID(data []byte, id string) []byte {
	line := fmt.Sprintf("%s = %q\n", InstanceIDKey, id)
	return append([]byte(line), data...)
}

func writeFileAtomic(path string, data []byte) error {
	perm := fs.FileMode(0o600)
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
		tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
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

// EnsureInstanceID 返回本机稳定的实例 ID：已配置则原样返回（校验
// 安全字母表），缺失时生成 "<机器名>-<随机哈希>" 并原子写回 exe 同
// 目录的配置文件。
//
// 跨进程首次初始化用系统 temp 目录下的 O_EXCL 锁保护，拿到锁后重读
// 一次，保证并发启动读到同一个 ID。写文件前保留原有 TOML 内容与
// 权限（新文件 0600）。
func EnsureInstanceID() (string, error) {
	path, err := instanceFile()
	if err != nil {
		return "", err
	}
	id, err := readInstanceID(path)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	unlock, err := lockInstanceInit(path)
	if err != nil {
		return "", err
	}
	defer unlock()
	if id, err = readInstanceID(path); err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	if id, err = newInstanceID(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("read config %s: %w", path, err)
		}
		// 文件缺失：以默认配置为原始内容，生成的文件本身就是完整配置。
		data = []byte(DefaultConfig)
	}
	if err := writeFileAtomic(path, withInstanceID(data, id)); err != nil {
		return "", err
	}
	return id, nil
}

func SeedDefaultConfig(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	unlock, err := lockInstanceInit(path)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := writeFileAtomic(path, []byte(DefaultConfig)); err != nil {
		return fmt.Errorf("seed config %s: %w", path, err)
	}
	return nil
}

func lockInstanceInit(path string) (func(), error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path %s: %w", path, err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	lockPath := filepath.Join(os.TempDir(), "fastimpl-instance-"+hex.EncodeToString(sum[:8])+".lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			fmt.Fprintf(f, "pid=%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create instance lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("instance lock %s is held by another fastimpl; if no other fastimpl is starting, delete it", lockPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// StripInstanceID 返回去掉顶层 instance_id 的配置文本，供 -init 复制
// 全局配置时使用：机器 ID 属于本机，不该写进项目配置。只有确实含
// 顶层 instance_id 时才需要重新编码（其余内容按 TOML 解码语义保留），
// 没有该键时原样返回，避免重排用户的注释与格式。
func StripInstanceID(data []byte) ([]byte, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if _, ok := doc[InstanceIDKey]; !ok {
		return data, nil
	}
	delete(doc, InstanceIDKey)
	out, err := toml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode config without %s: %w", InstanceIDKey, err)
	}
	return out, nil
}
