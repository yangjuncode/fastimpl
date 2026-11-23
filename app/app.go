// Package app 是 fastimpl 的编排层：命令行与 GUI 两个前端共用同一条
// 运行管线。管线为：加载配置 → 用 glab/gh 建 issue 依赖图 → 准备集成
// 分支 worktree → 调度器分批跑 worker → 全部完成后交付合并请求。
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/github"
	"github.com/yangjuncode/fastimpl/gitlab"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/prompt"
	"github.com/yangjuncode/fastimpl/scheduler"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// Options 是命令行（或 GUI）解析后的一次运行选项。
type Options struct {
	// Print 为真时只读取并打印依赖图，不做任何写入。
	Print bool
	// JSONPath 非空时把完整依赖图（含 open/closed 状态）写到该 JSON 文件。
	JSONPath string
	// Parallelism 覆盖配置中的并发上限；0 表示不覆盖。
	Parallelism int
	// ConfigPath 是配置文件路径。
	ConfigPath string
	// TemplatePath 是 worker 提示词模板路径；空表示先查
	// <work-dir>/fastimpl.tmp.md，再查 <exe 目录>/fastimpl.tmp.md。
	TemplatePath string
	// RepoDir 是目标仓库主 checkout 目录；空表示从 work-dir 出发
	// 向上/向下各搜至多 8 级，唯一命中即用（worktree.FindRoot）。
	RepoDir string
	// TreeDir 是 worktree 父目录；空表示 <repo-dir>/.worktrees。
	TreeDir string
	// WorkDir 是 fastimpl 工作目录；空表示当前目录（打开目录）。
	WorkDir string
	// SpecIID 是 spec（父）issue 的 IID。
	SpecIID int
}

// Run 执行一次运行：
// work-dir 解析 → config.Load（分层覆盖）→ repo-dir 解析 → origin
// 解析 → forge 解析 → graph.Build（子任务 + blocked-by 传递闭包）→
// -json/-print 导出 → prompt.Load → tree-dir 解析 → EnsureIntegration
// 集成分支 worktree → SyncBase + push + EnsureMR → worker.Dial（hcgo）
// → scheduler.Run（就绪即派、完成即放）。
func Run(ctx context.Context, opts Options, stdout, stderr io.Writer) error {
	workDir, err := resolveWorkDir(opts.WorkDir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(workDir, opts.ConfigPath)
	if err != nil {
		return err
	}
	if opts.Parallelism > 0 {
		cfg.Parallelism = opts.Parallelism
	}
	cfg.SpecIID = opts.SpecIID
	repoDir, err := resolveRepoDir(ctx, opts.RepoDir, cfg.RepoDir, workDir)
	if err != nil {
		return err
	}
	repo := worktree.NewRepo(repoDir, "")
	host, projectPath, err := repo.OriginCoordinate(ctx)
	if err != nil {
		return err
	}
	forge := resolveForge(ctx, cfg.Forge, host, repoDir)
	fc, g, err := buildGraph(ctx, forge, repoDir, host, projectPath, opts.SpecIID)
	if err != nil {
		return err
	}
	if opts.JSONPath != "" {
		if err := writeGraphJSON(g, opts.JSONPath); err != nil {
			return err
		}
	}
	if opts.Print {
		return g.WritePlan(stdout)
	}
	tmpl, err := prompt.Load(workDir, opts.TemplatePath)
	if err != nil {
		return err
	}
	treeDir, err := resolveTreeDir(ctx, opts.TreeDir, cfg.TreeDir, repoDir)
	if err != nil {
		return err
	}
	repo = worktree.NewRepo(repoDir, treeDir)
	// 终端命名前缀用 origin 项目路径的最后一段（gzdev/bf9100 → bf9100），
	// 多项目并行时避免 issue-<iid> 终端名互相碰撞。
	repo.Name = path.Base(projectPath)
	branch, intDir, baseName, err := repo.EnsureIntegration(ctx, opts.SpecIID, "", cfg.Integration)
	if err != nil {
		return err
	}
	// 复用的集成分支可能落后 base，先同步上游再推送。
	// base 同步冲突不阻塞本次运行：任务合并不受影响，冲突留给 MR。
	if err := repo.SyncBase(ctx, intDir, baseName); err != nil {
		fmt.Fprintf(stderr, "warning: sync %s into %s failed: %v\n", baseName, branch, err)
	}
	// 集成分支就位后立即推送并确保 MR 存在；此后每次集成再推送。
	if err := repo.Push(ctx, intDir, branch); err != nil {
		return fmt.Errorf("push integration branch %s: %w", branch, err)
	}
	mrURL, err := fc.EnsureMR(ctx, g.Spec().ProjectID, branch, baseName,
		fmt.Sprintf("spec #%d integration", opts.SpecIID))
	if err != nil {
		return fmt.Errorf("ensure merge request: %w", err)
	}
	agentArgv, err := cfg.AgentArgv()
	if err != nil {
		return err
	}
	runner, err := worker.Dial(ctx, cfg.HcBinary, cfg.HcSocket)
	if err != nil {
		return err
	}
	defer runner.Close()
	log := slog.New(slog.NewTextHandler(stderr, nil))
	log.Info("integration ready", "branch", branch, "mr", mrURL)
	sched := scheduler.New(cfg, repo, fc, g, runner, tmpl, intDir, branch, baseName, agentArgv, log)
	return sched.Run(ctx)
}

// resolveWorkDir 解析工作目录：空取当前目录，结果转绝对路径。
func resolveWorkDir(dir string) (string, error) {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}
		return cwd, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve work-dir %s: %w", dir, err)
	}
	return abs, nil
}

// resolveRepoDir 解析仓库目录：-repo-dir > 配置 repo_dir > FindRoot。
func resolveRepoDir(ctx context.Context, flagDir, cfgDir, workDir string) (string, error) {
	dir := flagDir
	if dir == "" {
		dir = cfgDir
	}
	if dir == "" {
		return worktree.FindRoot(workDir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve repo-dir %s: %w", dir, err)
	}
	return abs, nil
}

// resolveForge 解析 forge 类型：配置 > 主机名启发式 > glab/gh 探测 >
// 默认 gitlab。
func resolveForge(ctx context.Context, configured, host, dir string) string {
	if configured != "" {
		return configured
	}
	if forge := worktree.ForgeForHost(host); forge != "" {
		return forge
	}
	if gitlab.Probe(ctx, gitlab.ExecRunner{}, dir, host) {
		return config.ForgeGitLab
	}
	if github.Probe(ctx, github.ExecRunner{}, dir, host) {
		return config.ForgeGitHub
	}
	return config.ForgeGitLab
}

// resolveTreeDir 解析 worktree 父目录：-tree-dir > 配置 tree_dir >
// worktree.ResolveTreeDir 推断（既有 worktree 统一父目录 > .worktrees）。
func resolveTreeDir(ctx context.Context, flagDir, cfgDir, repoDir string) (string, error) {
	explicit := flagDir
	if explicit == "" {
		explicit = cfgDir
	}
	dir, err := worktree.ResolveTreeDir(ctx, repoDir, explicit)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve tree-dir %s: %w", dir, err)
	}
	return abs, nil
}

// buildGraph 按 forge 建 client 并构建完整依赖图；返回的 client
// 同时是调度器的 Forge 端口（Claim/Close/Note）。
func buildGraph(ctx context.Context, forge, dir, host, projectPath string, specIID int) (scheduler.Forge, *graph.Graph, error) {
	switch forge {
	case config.ForgeGitLab:
		client, err := gitlab.NewClient(ctx, gitlab.ExecRunner{}, dir, host, projectPath)
		if err != nil {
			return nil, nil, err
		}
		g, err := graph.Build(ctx, glabSource{c: client}, specIID)
		return client, g, err
	case config.ForgeGitHub:
		client, err := github.NewClient(ctx, github.ExecRunner{}, dir, host, projectPath)
		if err != nil {
			return nil, nil, err
		}
		g, err := graph.Build(ctx, ghSource{c: client}, specIID)
		return client, g, err
	default:
		return nil, nil, fmt.Errorf("unknown forge %q", forge)
	}
}

// writeGraphJSON 把依赖图写入 JSON 文件。
func writeGraphJSON(g *graph.Graph, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := g.WriteJSON(f); err != nil {
		return fmt.Errorf("write graph JSON %s: %w", path, err)
	}
	return nil
}
