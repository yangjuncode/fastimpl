package worker

import (
	"strings"
	"testing"
)

func TestParseReport(t *testing.T) {
	screen := `worker 输出……
FASTIMPL-READY tok1
implemented feature X
tests pass
`
	rep, err := ParseReport(screen, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Lines) != 2 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestParseReportWrongToken(t *testing.T) {
	screen := "FASTIMPL-READY other\nx\n"
	if _, err := ParseReport(screen, "tok1"); err == nil {
		t.Fatal("expected error for mismatched token")
	}
}

func TestParseReportSameLineBody(t *testing.T) {
	// agent 把报告正文写在令牌同一行（终端硬换行续行也算正文）。
	screen := `worker 输出……
FASTIMPL-READY tok1 实现：d2.go 已提交
验证：go build 通过
`
	rep, err := ParseReport(screen, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Lines) != 2 || rep.Lines[0] != "实现：d2.go 已提交" {
		t.Fatalf("report = %+v", rep)
	}
}

func TestParseReportTokenBoundary(t *testing.T) {
	// 令牌是同前缀更长令牌的前缀时不得误匹配。
	screen := "FASTIMPL-READY tok12\nx\n"
	if _, err := ParseReport(screen, "tok1"); err == nil {
		t.Fatal("expected boundary error")
	}
}

func TestParseReportLastFrame(t *testing.T) {
	// 屏幕上有多个历史帧时取最后一个。
	screen := "FASTIMPL-READY old\nx\n" +
		"FASTIMPL-READY tok2\ny\n"
	rep, err := ParseReport(screen, "tok2")
	if err != nil || len(rep.Lines) != 1 || rep.Lines[0] != "y" {
		t.Fatalf("report = %+v err=%v", rep, err)
	}
}

func TestNewTokenUnique(t *testing.T) {
	a, b := NewToken(), NewToken()
	if a == b || !strings.HasPrefix(a, "t") {
		t.Fatalf("tokens %q %q", a, b)
	}
}
