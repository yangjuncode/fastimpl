package prompt

import (
	"os"
	"path/filepath"
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

// TestDefaultTemplateReviewToggle require_review 开关第三方审核段：
// true 时包含 reviewer 与"两轮审核"，false 时整段不出现。
func TestDefaultTemplateReviewToggle(t *testing.T) {
	v := Vars{IssueNo: 1, Token: "t", Worktree: "/wt", WorkDir: "/wd", RepoDir: "/r", RequireReview: true, Reviewer: "swe-reviewer"}
	on, err := Render(DefaultTemplate, v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on, "使用 swe-reviewer 独立审核") || !strings.Contains(on, "两轮审核") {
		t.Fatalf("review-on render missing reviewer clause")
	}
	v.RequireReview = false
	off, err := Render(DefaultTemplate, v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "fusion-3rd-reviewer") || strings.Contains(off, "两轮审核") {
		t.Fatalf("review-off render still has reviewer clause")
	}
	if !strings.Contains(off, "验证后提交") || !strings.Contains(off, "验证全部通过") {
		t.Fatalf("review-off render broken: %s", off[:200])
	}
}

// TestSaveWorkDirCopy 验证提示词模板完整写入工作目录默认文件。
func TestSaveWorkDirCopy(t *testing.T) {
	work := t.TempDir()
	text := "custom prompt {{.IssueNo}}\n"
	path, err := SaveWorkDirCopy(work, text)
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
	if string(data) != text {
		t.Fatalf("saved prompt = %q, want %q", string(data), text)
	}
}

// reviewer 不是硬编码：默认模板通过 {{.Reviewer}} 填入配置值，允许
// gpt/gemini/swe 等不同审核后端共存。
func TestDefaultTemplateRendersConfiguredReviewer(t *testing.T) {
	out, err := Render(DefaultTemplate, Vars{RequireReview: true, Reviewer: "swe-reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "使用 swe-reviewer 独立审核") {
		t.Fatalf("configured reviewer missing from prompt: %s", out[:240])
	}
	if strings.Contains(out, "fusion-3rd-reviewer") {
		t.Fatalf("prompt still hard-codes the old reviewer: %s", out[:240])
	}
}
