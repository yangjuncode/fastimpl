package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// stubInstanceSeams 把实例 ID 的路径与主机名接缝指向测试夹具。
func stubInstanceSeams(t *testing.T, dir, host string) string {
	t.Helper()
	path := filepath.Join(dir, DefaultFile)
	origFile, origHost := instanceFile, instanceHostname
	instanceFile = func() (string, error) { return path, nil }
	instanceHostname = func() (string, error) { return host, nil }
	t.Cleanup(func() { instanceFile, instanceHostname = origFile, origHost })
	return path
}

// TestEnsureInstanceIDPreservesFile 首次生成后写回文件：保留原有注释与
// 末尾 table，ID 插在顶部而不是落进 table；已有文件保留权限。
func TestEnsureInstanceIDPreservesFile(t *testing.T) {
	dir := t.TempDir()
	path := stubInstanceSeams(t, dir, "build-node 01")
	original := "# 顶部注释\nparallelism = 3\n\n[board]\nstatus = \"In progress\"\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	id, err := EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if !validInstanceID(id) {
		t.Fatalf("generated id %q is not a safe instance id", id)
	}
	if !strings.HasPrefix(id, "build-node-01-") {
		t.Fatalf("id = %q, want the sanitized hostname prefix", id)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, `instance_id = "`+id+`"`+"\n") {
		t.Fatalf("instance_id is not at the top of the file:\n%s", text)
	}
	if !strings.Contains(text, `[board]`) || !strings.Contains(text, "# 顶部注释") {
		t.Fatalf("original content lost:\n%s", text)
	}
	var raw rawConfig
	if err := toml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("rewritten file is not valid TOML: %v\n%s", err, text)
	}
	if raw.Parallelism == nil || *raw.Parallelism != 3 {
		t.Fatalf("parallelism lost:\n%s", text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Fatalf("perm = %o, want preserved 640", perm)
	}
	// 再次调用应读到同一个 ID，不再改写文件。
	again, err := EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("second call = %q, want stable %q", again, id)
	}
}

// TestEnsureInstanceIDNewFileMode 新文件权限 0600，且只含 instance_id。
func TestEnsureInstanceIDNewFileMode(t *testing.T) {
	dir := t.TempDir()
	path := stubInstanceSeams(t, dir, "host")
	id, err := EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
	if got, err := readInstanceID(path); err != nil || got != id {
		t.Fatalf("readInstanceID = %q, %v; want %q", got, err, id)
	}
}

// TestEnsureInstanceIDConcurrent 并发首次启动只有一个 ID：互斥锁 +
// 拿到锁后重读，不出现两个进程各写一份的情况。
func TestEnsureInstanceIDConcurrent(t *testing.T) {
	dir := t.TempDir()
	stubInstanceSeams(t, dir, "host")
	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = EnsureInstanceID()
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
}

// TestEnsureInstanceIDRejectsUnsafe 显式 ID 必须落在安全字母表内。
func TestEnsureInstanceIDRejectsUnsafe(t *testing.T) {
	for _, id := range []string{"bad id", "bad/id", "bad\x00", strings.Repeat("a", maxInstanceIDLen+1)} {
		dir := t.TempDir()
		path := stubInstanceSeams(t, dir, "host")
		if err := os.WriteFile(path, []byte("instance_id = "+quote(id)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := EnsureInstanceID(); err == nil {
			t.Errorf("instance_id %q should be rejected", id)
		}
	}
	dir := t.TempDir()
	path := stubInstanceSeams(t, dir, "host")
	if err := os.WriteFile(path, []byte("instance_id = \"ok-1.2_3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := EnsureInstanceID()
	if err != nil || id != "ok-1.2_3" {
		t.Fatalf("EnsureInstanceID = %q, %v; want the configured id", id, err)
	}
}

// quote 把 id 写成 TOML 字符串（只用于测试夹具里的危险字符）。
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// TestStripInstanceID -init 复制全局配置时只去掉顶层机器 ID：table 字段、
// 引号键与多行字符串里的同名内容都保持 TOML 语义不变。
func TestStripInstanceID(t *testing.T) {
	in := "instance_id = \"box-1\"\nparallelism = 4\n\n[board]\ninstance_id = \"nested\"\n"
	out, err := StripInstanceID([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("stripped config is not valid TOML: %v\n%s", err, out)
	}
	if _, ok := doc[InstanceIDKey]; ok {
		t.Fatalf("top-level instance_id survived:\n%s", out)
	}
	if doc["parallelism"] != int64(4) {
		t.Fatalf("parallelism lost:\n%s", out)
	}
	board, ok := doc["board"].(map[string]any)
	if !ok || board[InstanceIDKey] != "nested" {
		t.Fatalf("nested table content changed: %+v\n%s", doc["board"], out)
	}

	// 没有顶层 instance_id 时原样返回（不重排注释与格式）。
	plain := "# 注释\nparallelism = 2\n\n[board]\ninstance_id = \"nested\"\n"
	same, err := StripInstanceID([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if string(same) != plain {
		t.Fatalf("content without a top-level id must be returned verbatim:\n%s", same)
	}

	// 引号键 + 多行字符串里的同名文本都要保持。
	quoted := "\"instance_id\" = \"box-2\"\nnote = \"\"\"\ninstance_id = \"inside\"\n\"\"\"\n"
	out, err = StripInstanceID([]byte(quoted))
	if err != nil {
		t.Fatal(err)
	}
	var qdoc map[string]any
	if err := toml.Unmarshal(out, &qdoc); err != nil {
		t.Fatalf("stripped config is not valid TOML: %v\n%s", err, out)
	}
	if _, ok := qdoc[InstanceIDKey]; ok {
		t.Fatalf("quoted top-level key survived:\n%s", out)
	}
	if qdoc["note"] != "instance_id = \"inside\"\n" {
		t.Fatalf("multiline string changed: %q", qdoc["note"])
	}

	if _, err := StripInstanceID([]byte("not = toml = at all\n")); err == nil {
		t.Fatal("invalid TOML must surface as an error")
	}
}

// TestLoadInstanceIDOnlyFromExeLayer instance_id 只从 exe 目录的全局层
// 读取：work-dir 与 -config 覆盖都无效。
func TestLoadInstanceIDOnlyFromExeLayer(t *testing.T) {
	exeDir := t.TempDir()
	orig := instanceFile
	instanceFile = func() (string, error) { return filepath.Join(exeDir, DefaultFile), nil }
	t.Cleanup(func() { instanceFile = orig })
	if err := os.WriteFile(filepath.Join(exeDir, DefaultFile), []byte("instance_id = \"exe-box\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, DefaultFile), []byte("instance_id = \"work-box\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(work, "custom.toml")
	if err := os.WriteFile(explicit, []byte("instance_id = \"flag-box\"\nparallelism = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubExeDir(t, exeDir)
	cfg, err := Load(work, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceID != "exe-box" {
		t.Fatalf("InstanceID = %q, want the exe-layer id", cfg.InstanceID)
	}
	cfg, err = Load(work, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstanceID != "exe-box" || cfg.Parallelism != 2 {
		t.Fatalf("cfg = %+v, want exe instance id and the explicit parallelism", cfg)
	}
}

// stubExeDir 把可执行文件目录接缝指向测试目录，让 Load 读该目录的
// 全局配置层。
func stubExeDir(t *testing.T, dir string) {
	t.Helper()
	orig := exeDir
	exeDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { exeDir = orig })
}

// TestSeedAfterIdentityKeepsID 身份写入之后再来一次 seed 不能截断/覆盖
// 已存在的文件：seed 与身份初始化共用同一把初始化锁，且只写缺失文件。
func TestSeedAfterIdentityKeepsID(t *testing.T) {
	dir := t.TempDir()
	path := stubInstanceSeams(t, dir, "box")
	id, err := EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), id) {
		t.Fatalf("seed overwrote the persisted instance id:\n%s", data)
	}
	if got, err := EnsureInstanceID(); err != nil || got != id {
		t.Fatalf("EnsureInstanceID = %q, %v; want the stable %q", got, err, id)
	}
}

// TestEnsureInstanceIDMissingFileSeedsDefaults 配置文件缺失时首次生成要把
// DefaultConfig 作为原始内容，产出的是完整可读配置而不仅是 ID。
func TestEnsureInstanceIDMissingFileSeedsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := stubInstanceSeams(t, dir, "box")
	id, err := EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw rawConfig
	if err := toml.Unmarshal(data, &raw); err != nil {
		t.Fatalf("generated config is not valid TOML: %v\n%s", err, data)
	}
	if raw.InstanceID == nil || *raw.InstanceID != id {
		t.Fatalf("generated instance_id = %v, want %q", raw.InstanceID, id)
	}
	// 生成的文件是完整配置：默认配置的注释仍在（不是只有一行 ID）。
	if !strings.Contains(string(data), "# parallelism") {
		t.Fatalf("generated config lacks the default content:\n%s", data)
	}
}

// TestNewInstanceIDBounded 超长主机名也被截断到安全上限内。
func TestNewInstanceIDBounded(t *testing.T) {
	dir := t.TempDir()
	stubInstanceSeams(t, dir, strings.Repeat("very-long-host-", 40))
	id, err := newInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) > maxInstanceIDLen {
		t.Fatalf("generated id length = %d, want <= %d (%q)", len(id), maxInstanceIDLen, id)
	}
	if !validInstanceID(id) {
		t.Fatalf("generated id %q is not a safe instance id", id)
	}
}
