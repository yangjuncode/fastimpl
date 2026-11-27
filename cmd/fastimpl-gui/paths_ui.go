package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/worktree"
)

// pickStartDir 为系统目录选择器决定起始目录：优先当前字段，其次回退到
// 给定目录（例如工作目录或仓库目录），避免每次从空路径打开对话框。
func pickStartDir(primary, fallback string) string {
	if p := strings.TrimSpace(primary); p != "" {
		return p
	}
	return strings.TrimSpace(fallback)
}

func (m *model) setRepoCandidates(next []string) {
	if sameStrings(m.repoCandidates, next) {
		return
	}
	m.repoCandidates = append([]string(nil), next...)
	m.candidatesRev.Update(func(n int) int { return n + 1 })
}

func (m *model) setTreeCandidates(next []string) {
	if sameStrings(m.treeCandidates, next) {
		return
	}
	m.treeCandidates = append([]string(nil), next...)
	m.candidatesRev.Update(func(n int) int { return n + 1 })
}

// selectWork 切换工作目录，清空依赖旧工作目录的仓库与 worktree 字段，
// 并按新工作目录重新发现（包括读取新目录下的 fastimpl.toml）。
func (m *model) selectWork(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	m.session.MarkAutoStarted()
	m.workText.Set(path)
	m.repoText.Set("")
	m.treeText.Set("")
	m.setRepoCandidates(nil)
	m.repoCandidatesWorkDir = ""
	m.setTreeCandidates(nil)
	m.treeCandidatesRepoDir = ""
	m.pathSaveHint.Set("")
	m.rediscover(path, "", "", false, false)
}

// selectRepo 选定仓库目录：清空旧的 Work Tree 字段并立即重新查找该仓库的
// worktree 目录；解析成功后若工作目录与仓库目录不一致，自动保存到
// <work-dir>/fastimpl.toml，下次在同一工作目录打开即可直接复用。
func (m *model) selectRepo(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	m.session.MarkAutoStarted()
	m.repoText.Set(path)
	m.treeText.Set("")
	m.setTreeCandidates(nil)
	m.treeCandidatesRepoDir = ""
	m.pathSaveHint.Set("")
	m.rediscover(strings.TrimSpace(m.workText.GetUntracked()), path, "", true, false)
}

// selectTree 选定 Work Tree 目录并重新校验；解析成功后持久化到
// <work-dir>/fastimpl.toml。
func (m *model) selectTree(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	m.session.MarkAutoStarted()
	m.treeText.Set(path)
	m.pathSaveHint.Set("")
	m.rediscover(strings.TrimSpace(m.workText.GetUntracked()), strings.TrimSpace(m.repoText.GetUntracked()), path, true, true)
}

// commitWorkInput 在用户编辑工作目录回车或失焦后触发重新发现。
func (m *model) commitWorkInput() {
	cur := strings.TrimSpace(m.workText.GetUntracked())
	if cur == "" || sameDir(cur, m.lastDiscoveredWork) {
		return
	}
	m.selectWork(cur)
}

// commitRepoInput 在用户手输仓库目录回车或失焦后重新查找 Work Tree 目录。
func (m *model) commitRepoInput() {
	cur := strings.TrimSpace(m.repoText.GetUntracked())
	if cur == "" || sameDir(cur, m.lastDiscoveredRepo) {
		return
	}
	m.selectRepo(cur)
}

// commitTreeInput 在用户手输 Work Tree 目录回车或失焦后重新校验并保存。
func (m *model) commitTreeInput() {
	cur := strings.TrimSpace(m.treeText.GetUntracked())
	if cur == "" || sameDir(cur, m.lastDiscoveredTree) {
		return
	}
	m.selectTree(cur)
}

// rediscover 发起新一轮本地路径预览；persist 为真时在该轮预览成功落盘配置。
func (m *model) rediscover(workDir, repoDir, treeDir string, persist, treeOnly bool) {
	opts := m.opts
	opts.WorkDir = workDir
	opts.RepoDir = repoDir
	opts.TreeDir = treeDir
	m.lastDiscoveredWork = workDir
	m.lastDiscoveredRepo = repoDir
	m.lastDiscoveredTree = treeDir
	seq := m.session.Discover(opts)
	if persist {
		m.persistSeq = seq
		m.persistTreeOnly = treeOnly
	} else {
		m.persistSeq = 0
		m.persistTreeOnly = false
	}
}

// syncPathDiscovery 在 sync 中同步候选目录列表，并在用户触发的路径解析完成后
// 把跨目录配置保存到 <work-dir>/fastimpl.toml。
func (m *model) syncPathDiscovery(snap snapshot) {
	if snap.Paths.WorkDir != "" {
		m.lastDiscoveredWork = snap.Paths.WorkDir
	}
	if snap.Paths.RepoDir != "" {
		m.lastDiscoveredRepo = snap.Paths.RepoDir
	}
	if snap.Paths.TreeDir != "" {
		m.lastDiscoveredTree = snap.Paths.TreeDir
	}

	if len(snap.RepoCandidates) > 0 {
		m.setRepoCandidates(snap.RepoCandidates)
		m.repoCandidatesWorkDir = snap.Paths.WorkDir
	} else if m.repoCandidatesWorkDir != "" && snap.Paths.WorkDir != "" && !sameDir(snap.Paths.WorkDir, m.repoCandidatesWorkDir) {
		// 仅在切换了工作目录时清空旧工作目录的候选仓库；同一工作目录下点选其中
		// 一个候选后保留列表，方便用户改选另一个候选仓库。
		m.setRepoCandidates(nil)
		m.repoCandidatesWorkDir = ""
	}

	if len(snap.TreeCandidates) > 0 {
		m.setTreeCandidates(snap.TreeCandidates)
		m.treeCandidatesRepoDir = snap.Paths.RepoDir
	} else if m.treeCandidatesRepoDir != "" && snap.Paths.RepoDir != "" && !sameDir(snap.Paths.RepoDir, m.treeCandidatesRepoDir) {
		m.setTreeCandidates(nil)
		m.treeCandidatesRepoDir = ""
	}

	if snap.DiscoverSeq != 0 && snap.DiscoverSeq == m.persistSeq && m.persistedSeq != snap.DiscoverSeq {
		m.persistedSeq = snap.DiscoverSeq
		m.persistResolvedPaths(snap)
	}
}

// persistResolvedPaths 在用户主动选择仓库或 Work Tree 目录且解析有效后，
// 写回 <work-dir>/fastimpl.toml。工作目录与仓库目录一致时只在用户显式挑选
// Work Tree 目录时写入 tree_dir，避免污染普通同目录项目。
func (m *model) persistResolvedPaths(snap snapshot) {
	if m.opts.ConfigPath != "" {
		return
	}
	if snap.PathErr != nil || snap.RepoPathErr != nil {
		return
	}
	workDir := strings.TrimSpace(snap.Paths.WorkDir)
	repoDir := strings.TrimSpace(snap.Paths.RepoDir)
	if workDir == "" || repoDir == "" || !isExistingDir(workDir) {
		return
	}
	treeDir := ""
	if snap.TreePathErr == nil {
		treeDir = strings.TrimSpace(snap.Paths.TreeDir)
	}
	if !sameDir(workDir, repoDir) {
		m.saveWorkDirPaths(workDir, repoDir, treeDir)
		return
	}
	if m.persistTreeOnly && treeDir != "" {
		m.saveWorkDirPaths(workDir, "", treeDir)
	}
}

// persistStartPaths 在用户点开始前做兜底保存：覆盖“输入仓库后直接点构建依赖图”
// 或“工作目录与自动发现的单一子仓库不一致”的场景；不覆盖命令行一次性 -repo-dir。
func (m *model) persistStartPaths(opts app.Options) {
	if m.opts.ConfigPath != "" {
		return
	}
	workDir := strings.TrimSpace(opts.WorkDir)
	repoDir := strings.TrimSpace(opts.RepoDir)
	treeDir := strings.TrimSpace(opts.TreeDir)
	if workDir == "" || repoDir == "" || sameDir(workDir, repoDir) {
		return
	}
	if m.initialOpts.RepoDir != "" && sameDir(m.initialOpts.RepoDir, repoDir) {
		return
	}
	if !isExistingDir(workDir) || !worktree.IsRepoRoot(repoDir) {
		return
	}
	if treeDir != "" && !filepath.IsAbs(treeDir) {
		treeDir = ""
	}
	m.saveWorkDirPaths(workDir, repoDir, treeDir)
}

// saveWorkDirPaths 调用 config.SaveWorkDirPaths 并去重，成功或失败都在表单提示区反馈。
func (m *model) saveWorkDirPaths(workDir, repoDir, treeDir string) {
	if sameDirOrBothEmpty(workDir, m.lastSavedWork) &&
		sameDirOrBothEmpty(repoDir, m.lastSavedRepo) &&
		sameDirOrBothEmpty(treeDir, m.lastSavedTree) {
		return
	}
	saved, err := config.SaveWorkDirPaths(workDir, repoDir, treeDir)
	if err != nil {
		m.pathSaveHint.Set("目录配置保存失败：" + attentionSummary(err.Error()))
		return
	}
	m.lastSavedWork = workDir
	m.lastSavedRepo = repoDir
	m.lastSavedTree = treeDir
	m.pathSaveHint.Set("已保存目录配置到 " + saved)
}

// repoCandidatesSection 在仓库目录输入框下方列出检测到的候选仓库。
func (m *model) repoCandidatesSection() core.Node {
	return m.pathCandidatesSection(
		"检测到多个仓库，点击选择",
		func() []string { return m.repoCandidates },
		m.repoText,
		func() string { return strings.TrimSpace(m.workText.GetUntracked()) },
		lucide.FolderGit2,
		m.selectRepo,
		"repo-candidate",
	)
}

// treeCandidatesSection 在 Work Tree 目录输入框下方列出检测到的候选父目录。
func (m *model) treeCandidatesSection() core.Node {
	return m.pathCandidatesSection(
		"检测到多个 Work Tree 目录，点击选择",
		func() []string { return m.treeCandidates },
		m.treeText,
		func() string { return strings.TrimSpace(m.workText.GetUntracked()) },
		lucide.FolderOpen,
		m.selectTree,
		"tree-candidate",
	)
}

// pathCandidatesSection 渲染一个可点击的候选目录列表；超过 4 项时自动滚动，
// 已选中的项用主色高亮。
func (m *model) pathCandidatesSection(
	title string,
	readList func() []string,
	current *core.Signal[string],
	baseDir func() string,
	idleIcon func() *core.SVG,
	onPick func(string),
	testIDPrefix string,
) core.Node {
	return core.Dynamic(func(cx *core.Scope) core.Node {
		m.candidatesRev.Get()
		candidates := readList()
		if len(candidates) == 0 {
			return nil
		}
		colors := ui.ColorsOf(cx)
		cur := strings.TrimSpace(current.Get())
		base := baseDir()
		buttons := make([]core.Node, 0, len(candidates))
		for i, path := range candidates {
			p := path
			selected := cur != "" && sameDir(cur, p)
			variant := ui.ButtonOutline
			icon := idleIcon
			if selected {
				variant = ui.ButtonPrimary
				icon = lucide.Check
			}
			buttons = append(buttons, ui.Button{
				Text:    candidateLabel(base, p),
				Variant: variant,
				Icon:    icon,
				Size:    ui.SizeSmall,
				Style:   core.Style{Width: core.Pct(100), JustifyContent: core.JustifyStart},
				OnPress: func() { onPick(p) },
				TestID:  fmt.Sprintf("%s-%d", testIDPrefix, i),
			})
		}
		listNode := core.Node(core.View{
			Style:    core.Style{Direction: core.Column, Gap: 6},
			Children: buttons,
		})
		if len(buttons) > 4 {
			listNode = core.View{
				Style: core.Style{Height: core.Pt(136), MinHeight: core.Pt(0)},
				Children: []core.Node{
					ui.ScrollView{
						Children:     buttons,
						ContentStyle: core.Style{Gap: 6, Padding: core.Edges{Right: 6}},
						TestID:       testIDPrefix + "-scroll",
					},
				},
			}
		}
		return core.View{
			TestID: testIDPrefix + "-list",
			Style:  core.Style{Direction: core.Column, Gap: 6, Padding: core.All(10), BorderWidth: 1, CornerRadius: 8},
			StyleFunc: func() core.Style {
				return core.Style{Background: colors.Background(), BorderColor: colors.Border()}
			},
			Children: []core.Node{
				ui.Text(fmt.Sprintf("%s（%d）", title, len(candidates)), 11, ui.Medium, colors.Muted),
				listNode,
			},
		}
	})
}

// pathSaveHintSection 展示自动保存目录配置后的反馈（例如已写入 fastimpl.toml）。
func (m *model) pathSaveHintSection() core.Node {
	colors := ui.ColorsOf(m.cx)
	return core.Show{
		When: func() bool { return m.pathSaveHint.Get() != "" },
		Then: func(*core.Scope) core.Node {
			return core.Text{
				ContentFunc: m.pathSaveHint.Get,
				MaxLines:    2,
				Style:       ui.TypeCaption.Merge(core.Style{TextAlign: core.TextAlignCenter}),
				StyleFunc: func() core.Style {
					if strings.HasPrefix(m.pathSaveHint.Get(), "目录配置保存失败") {
						return core.Style{Color: colors.Danger()}
					}
					return core.Style{Color: colors.Up()}
				},
				TestID: "form-save-hint",
			}
		},
	}
}

// candidateLabel 格式化候选目录按钮标签：位于工作目录内时优先展示相对路径，
// 避免长前缀截断仓库名；位于外部时展示完整路径。
func candidateLabel(baseDir, path string) string {
	if baseDir != "" {
		if rel, err := filepath.Rel(baseDir, path); err == nil && rel != "." && rel != "" && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return path
}

// sameDir 判断两个目录路径是否等价。
func sameDir(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	absA, absB = filepath.Clean(absA), filepath.Clean(absB)
	if absA == absB {
		return true
	}
	if realA, err := filepath.EvalSymlinks(absA); err == nil {
		absA = realA
	}
	if realB, err := filepath.EvalSymlinks(absB); err == nil {
		absB = realB
	}
	return absA == absB
}

func sameDirOrBothEmpty(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" && b == "" {
		return true
	}
	return sameDir(a, b)
}

func isExistingDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
