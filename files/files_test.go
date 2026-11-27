package files

import (
	"os"
	"path/filepath"
	"testing"
)

// WriteFileAtomic 的热加载读者只看到完整新文件；同时已有文件的权限
// 不能被默认 0644 覆盖（例如用户故意把配置改成 0600）。
func TestWriteFileAtomicPreservesExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fastimpl.toml")
	if err := os.WriteFile(path, []byte("parallelism = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("parallelism = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "parallelism = 2\n" {
		t.Fatalf("content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want existing 0600 preserved", got)
	}
}

func TestWriteFileAtomicCreatesNewFileWithRequestedPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fastimpl.prompt.md")
	if err := WriteFileAtomic(path, []byte("prompt"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want 0640", got)
	}
}
