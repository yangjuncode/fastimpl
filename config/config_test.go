package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestSaveWorkDirCopy 验证 GUI 保存的是当前生效配置的完整项目级副本：
// TOML 参数完整写入、instance_id 不进入工作目录层，重新加载后值保持一致。
func TestSaveWorkDirCopy(t *testing.T) {
	work := t.TempDir()
	cfg := Config{
		Parallelism:       4,
		Retries:           2,
		Forge:             ForgeGitHub,
		RepoDir:           work,
		TreeDir:           filepath.Join(work, ".worktrees"),
		AgentCmd:          `codex --model "o4 mini"`,
		Integration:       "spec-integration",
		RequireReview:     true,
		Reviewer:          "gemini-reviewer",
		HcSocket:          filepath.Join(work, "hc.sock"),
		TerminalRetention: "15m",
		LogFile:           "fastimpl-run.log",
		InstanceID:        "machine-1",
	}
	path, err := SaveWorkDirCopy(work, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(work, DefaultFile) {
		t.Fatalf("path = %q, want %q", path, filepath.Join(work, DefaultFile))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"parallelism = 4", "retries = 2", `forge = 'github'`, "repo_dir", "tree_dir",
		"agent_cmd", "integration", "require_review = true", "reviewer = 'gemini-reviewer'", "hc_socket", "hc_binary", "terminal_retention", "log_file",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("saved config does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "instance_id =") {
		t.Errorf("saved config must not persist instance_id into the work-dir layer:\n%s", text)
	}
	loaded, err := Load(work, path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Parallelism != cfg.Parallelism || loaded.Retries != cfg.Retries || loaded.Forge != cfg.Forge ||
		loaded.RepoDir != cfg.RepoDir || loaded.TreeDir != cfg.TreeDir || loaded.AgentCmd != cfg.AgentCmd ||
		loaded.Integration != cfg.Integration || loaded.RequireReview != cfg.RequireReview ||
		loaded.HcSocket != cfg.HcSocket || loaded.HcBinary != cfg.HcBinary || loaded.TerminalRetention != cfg.TerminalRetention ||
		loaded.LogFile != cfg.LogFile || loaded.ReviewerName() != cfg.ReviewerName() {
		t.Fatalf("loaded config = %+v, want the saved effective values %+v", loaded, cfg)
	}
}

// TestSaveWorkDirCopyRejectsInvalid 验证非法配置不会写入工作目录。
func TestSaveWorkDirCopyRejectsInvalid(t *testing.T) {
	work := t.TempDir()
	_, err := SaveWorkDirCopy(work, Config{Parallelism: 1, HcBinary: "hc", HcSocket: "/tmp/hc.sock"})
	if err == nil {
		t.Fatal("expected mutually exclusive hc settings to be rejected")
	}
	if _, statErr := os.Stat(filepath.Join(work, DefaultFile)); !os.IsNotExist(statErr) {
		t.Fatalf("invalid config was written anyway: stat err = %v", statErr)
	}
}

// TestParseTerminalRetention 验证单字段三种策略及非法输入。
func TestParseTerminalRetention(t *testing.T) {
	if d, never, err := ParseTerminalRetention(""); err != nil || never || d != 0 {
		t.Fatalf("empty = %v,%v,%v; want immediate", d, never, err)
	}
	if d, never, err := ParseTerminalRetention("0s"); err != nil || never || d != 0 {
		t.Fatalf("0s = %v,%v,%v; want immediate", d, never, err)
	}
	if d, never, err := ParseTerminalRetention("10m"); err != nil || never || d != 10*time.Minute {
		t.Fatalf("10m = %v,%v,%v; want 10m delayed", d, never, err)
	}
	if _, never, err := ParseTerminalRetention("Never"); err != nil || !never {
		t.Fatalf("Never = never=%v err=%v; want never", never, err)
	}
	for _, bad := range []string{"-1s", "abc", "forever"} {
		if err := (Config{Parallelism: 1, TerminalRetention: bad}).Validate(); err == nil {
			t.Errorf("terminal_retention %q must be rejected", bad)
		}
	}
}

// reviewer 是可配置的多模型审核后端：未配置时使用 gpt-reviewer，项目层可用
// reviewer = "gemini-reviewer"/"swe-reviewer" 等覆盖；空串表示回到默认。
func TestReviewerDefaultAndOverlay(t *testing.T) {
	work := t.TempDir()
	stubExeDir(t, t.TempDir())
	if got := (Config{}).ReviewerName(); got != "gpt-reviewer" {
		t.Fatalf("zero-value reviewer default = %q, want gpt-reviewer", got)
	}
	loadedDefault, err := Load(work, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedDefault.ReviewerName(); got != "gpt-reviewer" {
		t.Fatalf("loaded default reviewer = %q, want gpt-reviewer", got)
	}
	explicit := filepath.Join(work, "custom.toml")
	if err := os.WriteFile(explicit, []byte("reviewer = \"gemini-reviewer\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(work, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ReviewerName(); got != "gemini-reviewer" {
		t.Fatalf("overlay reviewer = %q, want gemini-reviewer", got)
	}
}
