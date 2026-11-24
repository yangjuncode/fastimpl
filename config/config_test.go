package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadExplicitOverlay 验证 -config 显式文件作为覆盖层：
// 只写的项覆盖，未写的项保留内置默认值。
func TestLoadExplicitOverlay(t *testing.T) {
	work := t.TempDir()
	explicit := filepath.Join(work, "custom.toml")
	if err := os.WriteFile(explicit, []byte("parallelism = 3\nretries = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(work, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Parallelism != 3 {
		t.Errorf("Parallelism = %d, want 3", cfg.Parallelism)
	}
	if cfg.Retries != 0 {
		t.Errorf("Retries = %d, want 0（显式零值不被默认值覆盖）", cfg.Retries)
	}
	if cfg.WorkDir != work {
		t.Errorf("WorkDir = %q, want %q", cfg.WorkDir, work)
	}
}

// TestLoadExplicitMissing 验证显式 -config 文件缺失时报错，不播种。
func TestLoadExplicitMissing(t *testing.T) {
	work := t.TempDir()
	if _, err := Load(work, filepath.Join(work, "nope.toml")); err == nil {
		t.Fatal("expected error for missing explicit config")
	}
}

// TestValidateForge 验证 forge 只接受 gitlab/github/空。
func TestValidateForge(t *testing.T) {
	for _, f := range []string{"", ForgeGitLab, ForgeGitHub} {
		if err := (Config{Parallelism: 1, Forge: f}).Validate(); err != nil {
			t.Errorf("forge %q: %v", f, err)
		}
	}
	if err := (Config{Parallelism: 1, Forge: "gitea"}).Validate(); err == nil {
		t.Error("forge gitea should be rejected")
	}
}

func TestAgentArgv(t *testing.T) {
	// 缺省：devin --permission-mode dangerous。
	argv, err := Config{}.AgentArgv()
	if err != nil || len(argv) != 3 || argv[0] != "devin" ||
		argv[1] != "--permission-mode" || argv[2] != "dangerous" {
		t.Fatalf("default argv = %v err=%v", argv, err)
	}
	// agent_cmd 覆盖：含引号与转义。
	argv, err = Config{AgentCmd: `codex --model "o4 mini" --flag 'a b' x\ y`}.AgentArgv()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--model", "o4 mini", "--flag", "a b", "x y"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %v)", i, argv[i], want[i], argv)
		}
	}
	// 未闭合引号报错。
	if _, err := (Config{AgentCmd: `devin "oops`}).AgentArgv(); err == nil {
		t.Fatal("expected error for unterminated quote")
	}
}
