// Package prompt 加载并渲染 worker 提示词模板。模板是文件，按
// files.Resolve 的约定查找：<work-dir>/fastimpl.prompt.md 优先
// （项目级定制），没有则用 <exe 同目录>/fastimpl.prompt.md（随程序
// 分发的默认模板）；-template 可显式指定其他文件。
package prompt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"text/template"

	"github.com/yangjuncode/fastimpl/files"
)

// DefaultFile 是默认模板文件名，先在 work-dir 查找，再在 exe 目录查找。
const DefaultFile = "fastimpl.prompt.md"

// DefaultTemplate 是模板文件都不存在时播种到 exe 目录的默认内容。
// 报告协议行（FASTIMPL-READY <token>）是 fastimpl 与 worker 的契约，
// 定制时不要破坏这一行的格式；提交以 worktree HEAD 为准。
const DefaultTemplate = `使用 implement 技能完成子任务 #{{.IssueNo}}。你的工作目录（cwd）是 fastimpl 的运行目录 {{.WorkDir}}，其中包含主仓库 checkout（{{.RepoDir}}）以及可能的其他目录，可自由查阅参考。本任务的代码产出目录是 git worktree {{.Worktree}}：所有新代码、修改、提交都在其中进行（可用 git -C {{.Worktree}}）；由 fastimpl 管理集成分支、推送、关闭 issue 和清理 worktree。读取任务正文和评论，先说明任务标题。只在 {{.Worktree}} 中修改和提交。完成实现与仓库要求的验证后，先由 leader 审核，再使用 fusion-3rd-reviewer-swe2max 独立审核。修复发现的问题，重新验证及审核后提交。只有实现、验证和两轮审核全部通过，且无未解决事项时，才在最终回复末尾按以下协议报告：单独一行由 FASTIMPL-READY、一个空格和令牌 {{.Token}} 拼接；随后最多六行，分别说明实现、验证结果、leader 审核、独立审核及阻塞情况。报告正文只包含上述结论，不复制代码、命令日志、凭据或 issue 原文。未达到条件则说明阻塞及剩余工作，不输出报告标记。
`

// Load 读取模板文件并返回文本。
// 未显式指定时按 files.Resolve 的顺序查找；两处都没有时把
// DefaultTemplate 播种到 exe 目录（共享默认模板就位，用户之后
// 可就地定制）。显式给出的 path 缺失时直接报错，不替用户创建。
func Load(workDir, path string) (string, error) {
	exeDir, err := files.ExeDir()
	if err != nil {
		return "", err
	}
	resolved := files.Resolve(workDir, exeDir, DefaultFile, path)
	data, err := os.ReadFile(resolved)
	if err == nil {
		return string(data), nil
	}
	if path != "" || !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read template %s: %w", resolved, err)
	}
	if werr := os.WriteFile(resolved, []byte(DefaultTemplate), 0o644); werr != nil {
		return "", fmt.Errorf("seed template %s: %w", resolved, werr)
	}
	return DefaultTemplate, nil
}

// Vars 是模板渲染变量。
type Vars struct {
	// IssueNo 是子任务 issue IID，模板写作 {{.IssueNo}}。
	IssueNo int
	// Token 是本次运行的报告令牌，写作 {{.Token}}。
	Token string
	// SpecNo 是 spec issue IID，写作 {{.SpecNo}}。
	SpecNo int
	// Worktree 是本任务的 git worktree 绝对路径，写作 {{.Worktree}}；
	// agent 的代码产出目录（与启动目录 work-dir 不同）。
	Worktree string
	// WorkDir 是 fastimpl 的运行目录（含主仓库 checkout 及可能的
	// 其他目录），写作 {{.WorkDir}}；也是 agent 进程的启动目录。
	WorkDir string
	// RepoDir 是目标仓库主 checkout 的绝对路径，写作 {{.RepoDir}}；
	// 供 agent 参考现有代码，不在其中修改。
	RepoDir string
}

// legacyReplacer 把早期占位符写法 {issue-no}/{token}/{spec-no}/
// {worktree}/{work-dir} 改写成等价的 text/template 动作，
// 让已播种/已定制的旧模板继续可用。
var legacyReplacer = strings.NewReplacer(
	"{issue-no}", "{{.IssueNo}}",
	"{token}", "{{.Token}}",
	"{spec-no}", "{{.SpecNo}}",
	"{worktree}", "{{.Worktree}}",
	"{work-dir}", "{{.WorkDir}}",
	"{repo-dir}", "{{.RepoDir}}",
)

// Render 用 text/template 渲染模板；除 {{.Field}} 外可使用 if/range 等
// 全部动作语法，旧式 {x} 占位符先统一改写为 {{.Field}} 再解析。
func Render(tmpl string, v Vars) (string, error) {
	t, err := template.New("prompt").Parse(legacyReplacer.Replace(tmpl))
	if err != nil {
		return "", fmt.Errorf("parse prompt template: %w", err)
	}
	var b strings.Builder
	if err := t.Execute(&b, v); err != nil {
		return "", fmt.Errorf("render prompt template: %w", err)
	}
	return b.String(), nil
}
