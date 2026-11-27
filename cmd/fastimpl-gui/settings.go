package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nsgo-dev/nsgo/base"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/prompt"
)

const (
	settingsTabConfig = "config"
	settingsTabPrompt = "prompt"
)

// settingsModel 是设置对话框的编辑状态。所有信号都只在界面线程读写；
// loaded 保留加载时的完整生效配置，保存时只覆盖表单里暴露的字段。
type settingsModel struct {
	tab    *core.Signal[string]
	dirty  *core.Signal[bool]
	status *core.Signal[string]
	err    *core.Signal[string]

	parallelism       *core.Signal[string]
	retries           *core.Signal[string]
	terminalRetention *core.Signal[string]
	logFile           *core.Signal[string]
	forge             *core.Signal[string]
	repoDir           *core.Signal[string]
	treeDir           *core.Signal[string]
	agentCmd          *core.Signal[string]
	integration       *core.Signal[string]
	requireReview     *core.Signal[bool]
	reviewer          *core.Signal[string]
	hcBinary          *core.Signal[string]
	hcSocket          *core.Signal[string]
	instanceID        *core.Signal[string]

	promptText *core.Signal[string]
	loaded     config.Config
}

// newSettingsModel 创建设置编辑状态。
func newSettingsModel(cx *core.Scope) *settingsModel {
	return &settingsModel{
		tab:               core.NewSignal(cx, settingsTabConfig),
		dirty:             core.NewSignal(cx, false),
		status:            core.NewSignal(cx, ""),
		err:               core.NewSignal(cx, ""),
		parallelism:       core.NewSignal(cx, ""),
		retries:           core.NewSignal(cx, ""),
		terminalRetention: core.NewSignal(cx, ""),
		logFile:           core.NewSignal(cx, ""),
		forge:             core.NewSignal(cx, ""),
		repoDir:           core.NewSignal(cx, ""),
		treeDir:           core.NewSignal(cx, ""),
		agentCmd:          core.NewSignal(cx, ""),
		integration:       core.NewSignal(cx, ""),
		requireReview:     core.NewSignal(cx, false),
		reviewer:          core.NewSignal(cx, ""),
		hcBinary:          core.NewSignal(cx, ""),
		hcSocket:          core.NewSignal(cx, ""),
		instanceID:        core.NewSignal(cx, ""),
		promptText:        core.NewSignal(cx, ""),
	}
}

// openSettings 打开设置对话框。未保存的编辑不丢：已有脏内容时直接打开，
// 否则从当前工作目录重新加载生效配置与提示词。
func (m *model) openSettings() {
	if m.settings == nil {
		m.settings = newSettingsModel(m.cx)
	}
	if !m.settings.dirty.GetUntracked() {
		m.loadSettings()
	}
	m.settingsOpen.Set(true)
}

// settingsWorkDir 返回设置保存到的 fastimpl 工作目录：优先使用启动表单
// 里的工作目录，其次是命令行 -work-dir，最后是进程当前目录。
func (m *model) settingsWorkDir() string {
	dir := strings.TrimSpace(m.workText.GetUntracked())
	if dir == "" {
		dir = strings.TrimSpace(m.opts.WorkDir)
	}
	if dir == "" {
		if cwd, err := os.Getwd(); err == nil {
			dir = cwd
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// loadSettings 读取当前生效配置与提示词。配置仍按既有分层规则解析；
// 保存时则固定写一份完整副本到当前工作目录。
func (m *model) loadSettings() {
	st := m.settings
	st.err.Set("")
	st.status.Set("")
	workDir := m.settingsWorkDir()
	var problems []string

	cfg, err := config.Load(workDir, m.opts.ConfigPath)
	if err != nil {
		problems = append(problems, "配置："+err.Error())
	} else {
		st.loaded = cfg
		st.parallelism.Set(strconv.Itoa(cfg.Parallelism))
		st.retries.Set(strconv.Itoa(cfg.Retries))
		st.terminalRetention.Set(cfg.TerminalRetention)
		st.logFile.Set(cfg.LogFile)
		st.forge.Set(cfg.Forge)
		st.repoDir.Set(cfg.RepoDir)
		st.treeDir.Set(cfg.TreeDir)
		st.agentCmd.Set(cfg.AgentCmd)
		st.integration.Set(cfg.Integration)
		st.requireReview.Set(cfg.RequireReview)
		st.reviewer.Set(cfg.ReviewerName())
		st.hcBinary.Set(cfg.HcBinary)
		st.hcSocket.Set(cfg.HcSocket)
		st.instanceID.Set(cfg.InstanceID)
		// 仓库错误打开设置时，把本次被拒绝/输入的目录预填到 repo_dir，
		// 用户可以直接修正它；这只是表单初值，不标脏、不写配置。
		if repoErr := m.snapshot().RepoError; repoErr != nil {
			suggestion := strings.TrimSpace(m.repoText.GetUntracked())
			if suggestion == "" {
				suggestion = strings.TrimSpace(repoErr.Dir)
			}
			if suggestion != "" {
				st.repoDir.Set(suggestion)
			}
		}
	}

	tmpl, err := prompt.Load(workDir, m.opts.TemplatePath)
	if err != nil {
		problems = append(problems, "提示词："+err.Error())
	} else {
		st.promptText.Set(tmpl)
	}

	if len(problems) > 0 {
		st.err.Set(strings.Join(problems, "\n"))
	} else {
		st.status.Set("已加载当前生效配置；保存会写入 " + workDir)
	}
	st.dirty.Set(false)
}

// markSettingsDirty 记录用户编辑，并清掉上一次保存结果。
func (m *model) markSettingsDirty() {
	st := m.settings
	st.dirty.Set(true)
	st.status.Set("")
	st.err.Set("")
}

// editText 返回一个文本输入的 OnChange：更新信号并标脏。
func (m *model) editText(sig *core.Signal[string]) func(string) {
	return func(text string) {
		sig.Set(text)
		m.markSettingsDirty()
	}
}

// editBool 返回一个布尔输入的 OnChange：更新信号并标脏。
func (m *model) editBool(sig *core.Signal[bool]) func(bool) {
	return func(value bool) {
		sig.Set(value)
		m.markSettingsDirty()
	}
}

// parseSettingsInt 解析一个必填整数字段。
func parseSettingsInt(text, name string, min int) (int, error) {
	trimmed := strings.TrimSpace(text)
	value, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，当前是 %q", name, trimmed)
	}
	if value < min {
		return 0, fmt.Errorf("%s 不能小于 %d，当前是 %d", name, min, value)
	}
	return value, nil
}

// configFromForm 把配置表单读回完整配置。未在表单中出现的字段沿用
// loadSettings 时的生效值（例如 ResumeBudget 与只读的 InstanceID）。
func (m *model) configFromForm() (config.Config, error) {
	st := m.settings
	cfg := st.loaded
	parallelism, err := parseSettingsInt(st.parallelism.GetUntracked(), "parallelism", 1)
	if err != nil {
		return config.Config{}, err
	}
	retries, err := parseSettingsInt(st.retries.GetUntracked(), "retries", 0)
	if err != nil {
		return config.Config{}, err
	}
	cfg.Parallelism = parallelism
	cfg.Retries = retries
	cfg.TerminalRetention = strings.TrimSpace(st.terminalRetention.GetUntracked())
	cfg.LogFile = strings.TrimSpace(st.logFile.GetUntracked())
	cfg.Forge = strings.TrimSpace(st.forge.GetUntracked())
	cfg.RepoDir = strings.TrimSpace(st.repoDir.GetUntracked())
	cfg.TreeDir = strings.TrimSpace(st.treeDir.GetUntracked())
	cfg.AgentCmd = strings.TrimSpace(st.agentCmd.GetUntracked())
	cfg.Integration = strings.TrimSpace(st.integration.GetUntracked())
	cfg.RequireReview = st.requireReview.GetUntracked()
	cfg.Reviewer = strings.TrimSpace(st.reviewer.GetUntracked())
	cfg.HcBinary = strings.TrimSpace(st.hcBinary.GetUntracked())
	cfg.HcSocket = strings.TrimSpace(st.hcSocket.GetUntracked())
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

// validatePromptText 在保存前确认模板能解析并用零值变量执行，避免把
// 运行时才会失败的模板写进项目配置。
func validatePromptText(text string) error {
	_, err := prompt.Render(text, prompt.Vars{})
	return err
}

// saveSettings 把当前生效的完整配置与提示词各保存一份到当前工作目录。
// 两个文件都写：配置是 TOML 完整副本，提示词是 Markdown 模板完整副本。
// 返回是否完整保存成功；失败原因已经放进设置对话框。
func (m *model) saveSettings() bool {
	st := m.settings
	st.err.Set("")
	st.status.Set("")
	cfg, err := m.configFromForm()
	if err != nil {
		st.err.Set("配置无法保存：" + err.Error())
		return false
	}
	promptText := st.promptText.GetUntracked()
	if err := validatePromptText(promptText); err != nil {
		st.err.Set("提示词模板无法保存：" + err.Error())
		return false
	}
	workDir := m.settingsWorkDir()
	configPath, err := config.SaveWorkDirCopy(workDir, cfg)
	if err != nil {
		st.err.Set("配置无法保存：" + err.Error())
		return false
	}
	promptPath, err := prompt.SaveWorkDirCopy(workDir, promptText)
	if err != nil {
		st.err.Set("提示词无法保存：" + err.Error())
		return false
	}
	st.loaded = cfg
	st.dirty.Set(false)
	st.status.Set("已保存到当前工作目录：\n" + configPath + "\n" + promptPath)
	return true
}

// requestCloseSettings 关闭设置对话框。若有未保存修改，关闭时自动保存；
// 校验或写入失败则保持打开并显示错误，让用户修正或明确放弃修改。
func (m *model) requestCloseSettings() {
	st := m.settings
	if st == nil || !st.dirty.GetUntracked() {
		m.settingsOpen.Set(false)
		return
	}
	if m.saveSettings() {
		m.settingsOpen.Set(false)
	}
}

// discardSettings 放弃未保存修改并关闭；下一次打开会重新从磁盘加载。
func (m *model) discardSettings() {
	st := m.settings
	if st != nil {
		st.dirty.Set(false)
		st.err.Set("")
		st.status.Set("")
	}
	m.settingsOpen.Set(false)
}

// retryWithSettingsRepo 用设置表单里的 repo_dir 作为本次运行的仓库目录
// 重试。这个按钮也会关闭对话框，因此与其他关闭路径一样先自动保存脏修改；
// 校验或写入失败时保持打开，避免用户误以为设置已落到工作目录。
func (m *model) retryWithSettingsRepo() {
	if m.settings == nil {
		return
	}
	repo := strings.TrimSpace(m.settings.repoDir.GetUntracked())
	if m.settings.dirty.GetUntracked() && !m.saveSettings() {
		return
	}
	if repo != "" {
		m.repoText.Set(repo)
	}
	m.settingsOpen.Set(false)
	m.start()
}

// settingsDialog 是设置对话框：配置参数与提示词两个 Tab。运行期仓库
// 错误也在这里展示，因为 repo_dir 正是配置 Tab 的一项。
func (m *model) settingsDialog() core.Node {
	st := m.settings
	return ui.Dialog{
		Open: m.settingsOpen.Get,
		OnOpenChange: func(open bool, _ base.Reason) {
			if open {
				m.settingsOpen.Set(true)
			} else {
				m.requestCloseSettings()
			}
		},
		Title:       "设置",
		Description: "修改后关闭对话框会自动保存；也可以点击保存立即写入当前工作目录。后续新 worker 与续写重试会读取可热更新项。",
		TestID:      "settings",
		Style:       core.Style{Width: core.Pt(760)},
		Children: []core.Node{
			m.settingsRepoError(),
			ui.Tabs[string]{
				Options: []ui.Option[string]{
					{Value: settingsTabConfig, Label: "配置", Icon: lucide.SlidersHorizontal, TestID: "settings-tab-config"},
					{Value: settingsTabPrompt, Label: "提示词", Icon: lucide.FileSliders, TestID: "settings-tab-prompt"},
				},
				Value: st.tab.Get, OnChange: st.tab.Set, Size: ui.SizeSmall, Label: "设置类别", TestID: "settings-tabs",
			},
			core.Show{
				When: func() bool { return st.tab.Get() == settingsTabConfig },
				Then: func(*core.Scope) core.Node { return m.configSettingsTab() },
				Else: func(*core.Scope) core.Node { return m.promptSettingsTab() },
			},
			m.settingsFeedback(),
		},
		Footer: []core.Node{
			core.Show{When: func() bool { return m.snapshot().RepoError != nil }, Then: func(*core.Scope) core.Node {
				return ui.Button{Text: "用此 repo_dir 重试", Variant: ui.ButtonSecondary, Icon: lucide.RefreshCw,
					OnPress: m.retryWithSettingsRepo, TestID: "settings-retry"}
			}},
			core.Show{When: func() bool { return st.dirty.Get() || st.err.Get() != "" }, Then: func(*core.Scope) core.Node {
				return ui.Button{Text: "放弃修改", Variant: ui.ButtonDanger, OnPress: m.discardSettings, TestID: "settings-discard"}
			}},
			ui.Button{Text: "关闭", Variant: ui.ButtonGhost, OnPress: m.requestCloseSettings, TestID: "settings-close"},
			ui.Button{Text: "保存到工作目录", Variant: ui.ButtonPrimary, Icon: lucide.Save, OnPress: func() { m.saveSettings() }, TestID: "settings-save"},
		},
	}
}

// settingsRepoError 在仓库解析失败时提示原因；配置 Tab 里的 repo_dir
// 就是修复它的持久化入口。
func (m *model) settingsRepoError() core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.Show{When: func() bool { return m.snapshot().RepoError != nil }, Then: func(*core.Scope) core.Node {
		return core.Text{ContentFunc: func() string {
			if err := m.snapshot().RepoError; err != nil {
				return "仓库目录错误：" + err.Error()
			}
			return ""
		}, MaxLines: 4, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Danger), TestID: "settings-repo-error"}
	}}
}

// settingsInputRow 是配置表单的一行文本输入。
func (m *model) settingsInputRow(label, placeholder string, sig *core.Signal[string], testID string) core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.View{TestID: testID + "-row", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8},
		Children: []core.Node{
			core.View{Style: core.Style{Width: core.Pt(118)}, Children: []core.Node{ui.Text(label, 11, ui.Medium, colors.Muted)}},
			core.View{Style: core.Style{FlexGrow: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
				ui.Input{Value: sig.Get, OnChange: m.editText(sig), Placeholder: placeholder, TestID: testID}}},
		}}
}

// configSettingsTab 是 TOML 配置参数 Tab。instance_id 只读展示：它是
// 本机全局标识，工作目录副本不能覆盖它。
func (m *model) configSettingsTab() core.Node {
	colors := ui.ColorsOf(m.cx)
	st := m.settings
	rows := []core.Node{
		m.settingsInputRow("parallelism", "同时运行的 worker 上限", st.parallelism, "settings-parallelism"),
		m.settingsInputRow("retries", "首次尝试之外的续写重试次数", st.retries, "settings-retries"),
		m.settingsInputRow("retention", "终端保留：0s 立即，10m 延迟，never 不销毁", st.terminalRetention, "settings-terminal-retention"),
		m.settingsInputRow("log_file", "运行日志文件；空不保存，保存后下一次运行生效", st.logFile, "settings-log-file"),
		settingsSelectRow(m, "forge", "上游 forge", st.forge, "settings-forge"),
		m.settingsInputRow("repo_dir", "目标仓库主 checkout 绝对路径", st.repoDir, "settings-repo-dir"),
		m.settingsInputRow("tree_dir", "worktree 父目录绝对路径", st.treeDir, "settings-tree-dir"),
		m.settingsInputRow("agent_cmd", "worker 的 agent 命令行", st.agentCmd, "settings-agent-cmd"),
		m.settingsInputRow("integration", "集成分支名；空为 issue-<spec>-integration", st.integration, "settings-integration"),
		settingsCheckboxRow(m, "require_review", "提示词强制 leader + 第三方双审", st.requireReview, "settings-require-review"),
		m.settingsInputRow("reviewer", "第三方 reviewer；默认 gpt-reviewer，可填 gemini-reviewer/swe-reviewer", st.reviewer, "settings-reviewer"),
		m.settingsInputRow("hc_binary", "hc 命令；与 hc_socket 二选一", st.hcBinary, "settings-hc-binary"),
		m.settingsInputRow("hc_socket", "herdr-core Unix socket；与 hc_binary 二选一", st.hcSocket, "settings-hc-socket"),
		settingsReadonlyRow(m, "instance_id", st.instanceID, "settings-instance-id"),
		core.Text{Content: "instance_id 是本机全局标识，只从程序同目录的 fastimpl.toml 读取；保存到工作目录时会省略它。",
			MaxLines: 2, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)},
	}
	return core.View{TestID: "settings-config-tab", Style: core.Style{Height: core.Pt(392), MinHeight: core.Pt(0)},
		Children: []core.Node{ui.ScrollView{Children: rows, ContentStyle: core.Style{Gap: 10, Padding: core.Edges{Right: 8}}, TestID: "settings-config-scroll"}}}
}

// settingsSelectRow 是配置表单的一行下拉选择。
func settingsSelectRow(m *model, label, placeholder string, sig *core.Signal[string], testID string) core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.View{TestID: testID + "-row", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8},
		Children: []core.Node{
			core.View{Style: core.Style{Width: core.Pt(118)}, Children: []core.Node{ui.Text(label, 11, ui.Medium, colors.Muted)}},
			ui.Select[string]{Options: []ui.Option[string]{
				{Value: "", Label: "自动推断"},
				{Value: config.ForgeGitLab, Label: "GitLab"},
				{Value: config.ForgeGitHub, Label: "GitHub"},
			}, Value: sig.Get, OnChange: func(value string) {
				sig.Set(value)
				m.markSettingsDirty()
			}, Placeholder: placeholder, Label: label, TestID: testID, Style: core.Style{FlexGrow: 1}},
		}}
}

// settingsCheckboxRow 是配置表单的一行布尔开关。
func settingsCheckboxRow(m *model, label, text string, sig *core.Signal[bool], testID string) core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.View{TestID: testID + "-row", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8},
		Children: []core.Node{
			core.View{Style: core.Style{Width: core.Pt(118)}, Children: []core.Node{ui.Text(label, 11, ui.Medium, colors.Muted)}},
			ui.Checkbox{Checked: sig.Get, OnChange: m.editBool(sig), Text: text, Label: label, TestID: testID},
		}}
}

// settingsReadonlyRow 是配置表单的一行只读值。
func settingsReadonlyRow(m *model, label string, sig *core.Signal[string], testID string) core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.View{TestID: testID + "-row", Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8},
		Children: []core.Node{
			core.View{Style: core.Style{Width: core.Pt(118)}, Children: []core.Node{ui.Text(label, 11, ui.Medium, colors.Muted)}},
			core.View{Style: core.Style{FlexGrow: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
				ui.Input{Value: sig.Get, Readonly: true, Placeholder: "未设置", TestID: testID}}},
		}}
}

// promptSettingsTab 是提示词模板 Tab。
func (m *model) promptSettingsTab() core.Node {
	colors := ui.ColorsOf(m.cx)
	st := m.settings
	return core.View{TestID: "settings-prompt-tab", Style: core.Style{Height: core.Pt(392), MinHeight: core.Pt(0), Gap: 8},
		Children: []core.Node{
			core.Text{Content: "编辑 worker 提示词模板。保存后当前工作目录会有一份 fastimpl.prompt.md，之后该目录运行优先使用它。",
				MaxLines: 2, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)},
			ui.Input{Value: st.promptText.Get, OnChange: m.editText(st.promptText), Multiline: true,
				Style: core.Style{Height: core.Pt(342)}, Placeholder: "worker 提示词模板", TestID: "settings-prompt"},
		}}
}

// settingsFeedback 是加载/保存结果与错误说明。
func (m *model) settingsFeedback() core.Node {
	colors := ui.ColorsOf(m.cx)
	st := m.settings
	return core.Group{
		core.Show{When: func() bool { return st.err.Get() != "" }, Then: func(*core.Scope) core.Node {
			return core.Text{ContentFunc: st.err.Get, MaxLines: 4, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Danger), TestID: "settings-error"}
		}},
		core.Show{When: func() bool { return st.status.Get() != "" }, Then: func(*core.Scope) core.Node {
			return core.Text{ContentFunc: st.status.Get, MaxLines: 4, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Up), TestID: "settings-status"}
		}},
	}
}
