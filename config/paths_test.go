package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveWorkDirPathsCreatesMinimalOverride(t *testing.T) {
	globalDir := t.TempDir()
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "sub", "repo")
	treeDir := filepath.Join(repoDir, ".worktrees")

	prevExe := exeDir
	exeDir = func() (string, error) { return globalDir, nil }
	t.Cleanup(func() { exeDir = prevExe })

	// 全局配置设了 agent_cmd / parallelism / instance_id，工作目录配置只应写目录覆盖，
	// 不能把全局设置冻结复制过去。
	globalTOML := "parallelism = 12\nagent_cmd = \"custom-agent --fast\"\ninstance_id = \"box-1\"\n"
	if err := os.WriteFile(filepath.Join(globalDir, DefaultFile), []byte(globalTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	saved, err := SaveWorkDirPaths(workDir, repoDir, treeDir)
	if err != nil {
		t.Fatalf("SaveWorkDirPaths = %v", err)
	}
	wantPath := filepath.Join(workDir, DefaultFile)
	if saved != wantPath {
		t.Fatalf("saved path = %q, want %q", saved, wantPath)
	}

	rawBytes, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rawBytes)
	if strings.Contains(text, "parallelism") || strings.Contains(text, "agent_cmd") || strings.Contains(text, "instance_id") {
		t.Fatalf("SaveWorkDirPaths unexpectedly copied unrelated settings:\n%s", text)
	}

	cfg, err := Load(workDir, "")
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.RepoDir != repoDir || cfg.TreeDir != treeDir {
		t.Errorf("loaded paths = (%q, %q), want (%q, %q)", cfg.RepoDir, cfg.TreeDir, repoDir, treeDir)
	}
	if cfg.Parallelism != 12 || cfg.AgentCmd != "custom-agent --fast" || cfg.InstanceID != "box-1" {
		t.Errorf("global settings not inherited: %+v", cfg)
	}
}

func TestSaveWorkDirPathsPreservesExistingOverridesAndClearsStaleTreeDir(t *testing.T) {
	globalDir := t.TempDir()
	workDir := t.TempDir()
	repoA := filepath.Join(workDir, "repo-a")
	treeA := filepath.Join(workDir, "trees-a")
	repoB := filepath.Join(workDir, "repo-b")

	prevExe := exeDir
	exeDir = func() (string, error) { return globalDir, nil }
	t.Cleanup(func() { exeDir = prevExe })

	if err := os.WriteFile(filepath.Join(globalDir, DefaultFile), []byte("retries = 8\nrequire_review = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	existing := "retries = 0\nrequire_review = false\nlog_file = \"run.log\"\ninstance_id = \"bad\"\nrepo_dir = " +
		`"` + repoA + `"` + "\ntree_dir = " + `"` + treeA + `"` + "\n"
	if err := os.WriteFile(filepath.Join(workDir, DefaultFile), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	// 切换到 repoB 且尚未提供新 treeDir：应保留 retries=0/require_review=false/log_file，
	// 丢弃 instance_id，并清掉属于 repoA 的旧 tree_dir。
	if _, err := SaveWorkDirPaths(workDir, repoB, ""); err != nil {
		t.Fatalf("SaveWorkDirPaths = %v", err)
	}

	rawBytes, err := os.ReadFile(filepath.Join(workDir, DefaultFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawBytes), "instance_id") {
		t.Fatalf("instance_id leaked into work-dir config:\n%s", rawBytes)
	}

	cfg, err := Load(workDir, "")
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if cfg.RepoDir != repoB {
		t.Errorf("RepoDir = %q, want %q", cfg.RepoDir, repoB)
	}
	if cfg.TreeDir != "" {
		t.Errorf("TreeDir = %q, want stale tree_dir cleared on repo switch", cfg.TreeDir)
	}
	if cfg.Retries != 0 || cfg.RequireReview || cfg.LogFile != "run.log" {
		t.Errorf("existing overrides lost: retries=%d require_review=%v log_file=%q", cfg.Retries, cfg.RequireReview, cfg.LogFile)
	}
}

func TestSaveWorkDirPathsRejectsInvalidInputs(t *testing.T) {
	workDir := t.TempDir()
	if _, err := SaveWorkDirPaths("", "/abs/repo", ""); err == nil {
		t.Error("accepted empty workDir")
	}
	if _, err := SaveWorkDirPaths(workDir, "", ""); err == nil {
		t.Error("accepted empty repoDir and treeDir")
	}
	if _, err := SaveWorkDirPaths(workDir, "relative/repo", ""); err == nil {
		t.Error("accepted relative repoDir")
	}
	if _, err := SaveWorkDirPaths(workDir, "", "relative/tree"); err == nil {
		t.Error("accepted relative treeDir")
	}
}
