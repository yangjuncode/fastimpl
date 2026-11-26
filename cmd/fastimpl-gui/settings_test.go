package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/prompt"
)

// 设置保存：用户修改配置与提示词后，当前工作目录得到两份完整副本，
// 配置副本随后能被 config.Load 按项目层读回。
func TestSettingsSaveWritesWorkDirCopies(t *testing.T) {
	work := t.TempDir()
	gui, m, _ := headless(t, app.Options{})
	waitPaths(t, gui, m)
	m.workText.Set(work)
	m.openSettings()

	m.editText(m.settings.parallelism)("5")
	m.editText(m.settings.logFile)("run.log")
	m.editText(m.settings.reviewer)("swe-reviewer")
	m.editText(m.settings.promptText)("custom prompt {{.IssueNo}}\n")
	m.saveSettings()

	configPath := filepath.Join(work, config.DefaultFile)
	promptPath := filepath.Join(work, prompt.DefaultFile)
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("saved config missing: %v", err)
	}
	if !strings.Contains(string(configData), "parallelism = 5") {
		t.Fatalf("saved config does not contain the edited parallelism:\n%s", string(configData))
	}
	promptData, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("saved prompt missing: %v", err)
	}
	if string(promptData) != "custom prompt {{.IssueNo}}\n" {
		t.Fatalf("saved prompt = %q", string(promptData))
	}
	loaded, err := config.Load(work, "")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Parallelism != 5 {
		t.Fatalf("loaded parallelism = %d, want the GUI-saved 5", loaded.Parallelism)
	}
	if loaded.LogFile != "run.log" {
		t.Fatalf("loaded log_file = %q, want the GUI-saved run.log", loaded.LogFile)
	}
	if loaded.ReviewerName() != "swe-reviewer" {
		t.Fatalf("loaded reviewer = %q, want the GUI-saved swe-reviewer", loaded.ReviewerName())
	}
	if m.settings.dirty.GetUntracked() {
		t.Error("settings remain dirty after a successful save")
	}
	if status := m.settings.status.GetUntracked(); !strings.Contains(status, configPath) || !strings.Contains(status, promptPath) {
		t.Fatalf("save status = %q, want both written paths", status)
	}
}

// 非法提示词不能保存：不写入任何副本，错误留在设置对话框里。
func TestSettingsSaveRejectsInvalidPrompt(t *testing.T) {
	work := t.TempDir()
	gui, m, _ := headless(t, app.Options{})
	waitPaths(t, gui, m)
	m.workText.Set(work)
	m.openSettings()
	m.editText(m.settings.promptText)("broken {{")
	m.saveSettings()
	if errText := m.settings.err.GetUntracked(); !strings.Contains(errText, "提示词模板无法保存") {
		t.Fatalf("settings error = %q, want the prompt validation failure", errText)
	}
	for _, path := range []string{filepath.Join(work, config.DefaultFile), filepath.Join(work, prompt.DefaultFile)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s exists after a rejected save (stat err %v)", path, err)
		}
	}
}

// 配置表单沿用 config.Validate：hc_binary 与 hc_socket 互斥。
func TestConfigFromFormRejectsMutuallyExclusiveHc(t *testing.T) {
	gui, m, _ := headless(t, app.Options{})
	waitPaths(t, gui, m)
	m.openSettings()
	m.editText(m.settings.hcBinary)("hc")
	m.editText(m.settings.hcSocket)("/tmp/hc.sock")
	if _, err := m.configFromForm(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("configFromForm err = %v, want the hc mutual-exclusion error", err)
	}
}

// 关闭自动保存：有脏修改时 requestCloseSettings 先写工作目录副本，成功后关闭。
func TestSettingsCloseAutoSavesDirtyChanges(t *testing.T) {
	work := t.TempDir()
	gui, m, _ := headless(t, app.Options{})
	waitPaths(t, gui, m)
	m.workText.Set(work)
	m.openSettings()
	m.editText(m.settings.parallelism)("6")
	m.requestCloseSettings()
	if m.settingsOpen.GetUntracked() {
		t.Fatal("settings dialog stayed open after a successful close-time save")
	}
	data, err := os.ReadFile(filepath.Join(work, config.DefaultFile))
	if err != nil {
		t.Fatalf("close-time save did not write config: %v", err)
	}
	if !strings.Contains(string(data), "parallelism = 6") {
		t.Fatalf("close-time saved config missing edit:\n%s", string(data))
	}
	if _, err := os.Stat(filepath.Join(work, prompt.DefaultFile)); err != nil {
		t.Fatalf("close-time save did not write prompt copy: %v", err)
	}
}

// 关闭自动保存遇到非法输入：不保存、保持打开并显示错误；用户可以明确放弃修改。
func TestSettingsCloseWithInvalidInputStaysOpen(t *testing.T) {
	work := t.TempDir()
	gui, m, _ := headless(t, app.Options{})
	waitPaths(t, gui, m)
	m.workText.Set(work)
	m.openSettings()
	m.editText(m.settings.promptText)("broken {{")
	m.requestCloseSettings()
	if !m.settingsOpen.GetUntracked() {
		t.Fatal("settings dialog closed although the close-time save failed")
	}
	if errText := m.settings.err.GetUntracked(); !strings.Contains(errText, "提示词模板无法保存") {
		t.Fatalf("settings error = %q, want the validation failure", errText)
	}
	if _, err := os.Stat(filepath.Join(work, config.DefaultFile)); !os.IsNotExist(err) {
		t.Fatalf("invalid input still wrote config (stat err %v)", err)
	}
	m.discardSettings()
	if m.settingsOpen.GetUntracked() {
		t.Fatal("discard did not close the settings dialog")
	}
}

// “用此 repo_dir 重试”也是关闭路径：脏修改先自动保存，然后才把目录用于本次运行。
func TestSettingsRetryAutoSavesDirtyChanges(t *testing.T) {
	work := t.TempDir()
	gui, m, s := headless(t, app.Options{})
	waitPaths(t, gui, m)
	m.workText.Set(work)
	m.issueText.Set("12")
	m.openSettings()
	m.editText(m.settings.parallelism)("7")
	m.editText(m.settings.repoDir)(work)
	started := make(chan app.Options, 1)
	s.runFn = func(_ context.Context, opts app.Options, _ io.Writer, _ app.Observer) error {
		started <- opts
		return context.Canceled
	}
	m.retryWithSettingsRepo()
	select {
	case opts := <-started:
		if opts.RepoDir != work {
			t.Fatalf("retry RepoDir = %q, want %q", opts.RepoDir, work)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not start a run")
	}
	data, err := os.ReadFile(filepath.Join(work, config.DefaultFile))
	if err != nil {
		t.Fatalf("retry close-time save did not write config: %v", err)
	}
	if !strings.Contains(string(data), "parallelism = 7") {
		t.Fatalf("retry saved config missing edit:\n%s", string(data))
	}
}
