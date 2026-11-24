package prompt

import (
	"strings"
	"testing"
)

// TestRenderVars 全部字段经 {{.Field}} 渲染。
func TestRenderVars(t *testing.T) {
	out, err := Render("{{.IssueNo}}|{{.Token}}|{{.SpecNo}}|{{.Worktree}}|{{.WorkDir}}|{{.RepoDir}}|{{.IntBranch}}|{{.Branch}}",
		Vars{IssueNo: 7, Token: "t9", SpecNo: 3, Worktree: "/wt", WorkDir: "/wd", RepoDir: "/r",
			IntBranch: "issue-3-integration", Branch: "issue-7"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "7|t9|3|/wt|/wd|/r|issue-3-integration|issue-7" {
		t.Fatalf("out %q", out)
	}
}

// TestRenderTemplateActions 新式 {{.Field}} 与动作语法可用。
func TestRenderTemplateActions(t *testing.T) {
	out, err := Render("{{.IssueNo}}:{{if eq .WorkDir .RepoDir}}same{{else}}diff{{end}}",
		Vars{IssueNo: 9, WorkDir: "/a", RepoDir: "/a"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "9:same" {
		t.Fatalf("out %q", out)
	}
	out, err = Render("{{if eq .WorkDir .RepoDir}}same{{else}}diff{{end}}",
		Vars{WorkDir: "/a", RepoDir: "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "diff" {
		t.Fatalf("out %q", out)
	}
}

// TestDefaultTemplateBranch 默认模板按 WorkDir 是否等于 RepoDir 分支：
// 一致时叮嘱别在主 checkout 改动，不一致时说明运行目录含 checkout。
func TestDefaultTemplateBranch(t *testing.T) {
	same, err := Render(DefaultTemplate, Vars{
		IssueNo: 1, Token: "t", Worktree: "/wt", WorkDir: "/r", RepoDir: "/r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(same, "就是主仓库 checkout") || strings.Contains(same, "包含主仓库") {
		t.Fatalf("same-dir branch wrong: %s", same[:120])
	}
	diff, err := Render(DefaultTemplate, Vars{
		IssueNo: 1, Token: "t", Worktree: "/wt", WorkDir: "/wd", RepoDir: "/r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "包含主仓库 checkout（/r）") {
		t.Fatalf("diff-dir branch wrong: %s", diff[:120])
	}
}
