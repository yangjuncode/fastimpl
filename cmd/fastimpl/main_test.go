package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yangjuncode/fastimpl/version"
)

// -version 只把版本写到 stdout 并退出，不进入运行管线。
func TestRunVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(-version) = %v, want no error", err)
	}
	want := "fastimpl version " + version.String() + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want no startup line for -version", stderr.String())
	}
}

// 正常启动先打印版本。版本写到 stderr，stdout 仍只承载命令自己的结果。
func TestRunPrintsVersionOnStartup(t *testing.T) {
	work := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-init", "-work-dir", work}, &stdout, &stderr); err != nil {
		t.Fatalf("run(-init) = %v, want no error", err)
	}
	want := "fastimpl version " + version.String() + "\n"
	if !strings.HasPrefix(stderr.String(), want) {
		t.Fatalf("stderr prefix = %q, want version line %q", stderr.String(), want)
	}
	if strings.Contains(stdout.String(), "fastimpl version") {
		t.Fatalf("stdout contains the diagnostic version line: %q", stdout.String())
	}
	for _, name := range []string{"fastimpl.toml", "fastimpl.prompt.md"} {
		if _, err := os.Stat(filepath.Join(work, name)); err != nil {
			t.Fatalf("-init did not write %s: %v", name, err)
		}
	}
}

// parseArguments 的第三返回值是 error；第二返回值只表示用户是否请求 -version。
func TestParseArgumentsVersionFlag(t *testing.T) {
	_, show, err := parseArguments([]string{"-version"}, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("parseArguments(-version) = %v, want no error", err)
	}
	if !show {
		t.Fatal("parseArguments(-version) did not report the version request")
	}
}

// 无 spec 是合法命令行：是否可启动取决于 app 层是否扫描到 active run。
func TestParseArgumentsAllowsMissingSpecForActiveRecovery(t *testing.T) {
	opts, show, err := parseArguments(nil, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("parseArguments(nil) = %v, want no error", err)
	}
	if show {
		t.Fatal("show version = true without -version")
	}
	if opts.SpecIID != 0 {
		t.Fatalf("SpecIID = %d, want zero so app recovery scanning can choose one", opts.SpecIID)
	}
}

// CLI 中断确认的默认答案必须是 detach；只有用户明确输入 y/yes 才停止 worker。
func TestInterruptStopAnswerRequiresExplicitYes(t *testing.T) {
	for _, answer := range []string{"y", "Y", "yes", " YES \n"} {
		if !shouldStopWorkers(answer) {
			t.Errorf("shouldStopWorkers(%q) = false, want explicit yes accepted", answer)
		}
	}
	for _, answer := range []string{"", "n", "no", "detach", "yep"} {
		if shouldStopWorkers(answer) {
			t.Errorf("shouldStopWorkers(%q) = true, want default detach", answer)
		}
	}
}
