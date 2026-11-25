// Package app 是 fastimpl 的编排层：命令行与 GUI 两个前端共用同一条
// 运行管线。管线为：加载配置 → 用 glab/gh 建 issue 依赖图 → 准备集成
// 分支 worktree → 调度器分批跑 worker → 全部完成后交付合并请求。
// GUI 可在依赖图建好后插入确认门禁：确认前不准备集成分支、不连接
// hcgo、不启动任何 worker。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/files"
	"github.com/yangjuncode/fastimpl/forge"
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
	// Init 为真时把 fastimpl.toml 与 fastimpl.prompt.md 生成到
	// work-dir 后退出：exe 目录已有同名文件则复制，没有用内置默认。
	Init bool
	// Print 为真时只读取并打印依赖图，不做任何写入。
	Print bool
	// JSONPath 非空时把完整依赖图（含 open/closed 状态）写到该 JSON 文件。
	JSONPath string
	// Parallelism 覆盖配置中的并发上限；0 表示不覆盖。
	Parallelism int
	// ConfigPath 是配置文件路径。
	ConfigPath string
	// TemplatePath 是 worker 提示词模板路径；空表示先查
	// <work-dir>/fastimpl.prompt.md，再查 <exe 目录>/fastimpl.prompt.md。
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
	return run(ctx, opts, stdout, stderr, Observer{})
}

// RunObserved 是 Run 的共享实现入口，另带观察者：CLI 传零值
// Observer，GUI 用它跟随构图进度与调度状态。回调在运行自己的
// goroutine 上同步调用，界面实现需要自己转投到 UI 队列。
func RunObserved(ctx context.Context, opts Options, stdout, stderr io.Writer, observer Observer) error {
	return run(ctx, opts, stdout, stderr, observer)
}

// run 是 Run 与 RunObserved 共用的运行管线。
func run(ctx context.Context, opts Options, stdout, stderr io.Writer, observer Observer) error {
	workDir, err := resolveWorkDir(opts.WorkDir)
	if err != nil {
		return err
	}
	if opts.Init {
		return initFiles(workDir, stdout)
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	// 真正调度前先确保本机稳定实例 ID（跨机器认领用）：尽早生成，
	// 保证随后 config.Load 能读到它。只读的 -print 不生成。
	if !opts.Print {
		if _, err := config.EnsureInstanceID(); err != nil {
			return err
		}
	}
	cfg, err := config.Load(workDir, opts.ConfigPath)
	if err != nil {
		return err
	}
	if opts.Parallelism > 0 {
		cfg.Parallelism = opts.Parallelism
	}
	cfg.SpecIID = opts.SpecIID
	if observer.ConfigLoaded != nil {
		observer.ConfigLoaded(cfg)
	}
	repoDir, err := resolveRepoDir(ctx, opts.RepoDir, cfg.RepoDir, workDir)
	if err != nil {
		return repoError(opts.RepoDir, err)
	}
	if observer.observing() {
		// GUI 给的目录必须自己就是仓库顶层：git 在子目录里也能跑，
		// 会把父仓库的 origin 认成本项目的。CLI 保持既有行为。
		if err := verifyRepoRoot(ctx, repoDir); err != nil {
			return err
		}
	}
	log.Info("repository", "dir", repoDir, "work_dir", workDir)
	repo := worktree.NewRepo(repoDir, "")
	host, projectPath, err := repo.OriginCoordinate(ctx)
	if err != nil {
		return repoError(repoDir, err)
	}
	forge := resolveForge(ctx, cfg.Forge, host, repoDir)
	log.Info("building dependency graph", "forge", forge, "origin", host+"/"+projectPath, "spec", opts.SpecIID)
	buildStart := time.Now()
	fc, g, err := buildGraph(ctx, forge, repoDir, host, projectPath, opts.SpecIID, log, observer.Progress)
	if err != nil {
		return err
	}
	var open, closed int
	for _, n := range g.Snapshot() {
		if n.Closed {
			closed++
		} else {
			open++
		}
	}
	log.Info("graph built", "nodes", len(g.Snapshot()), "open", open, "closed", closed,
		"elapsed", time.Since(buildStart).Round(time.Millisecond))
	if observer.GraphReady != nil {
		observer.GraphReady(g)
	}
	if opts.JSONPath != "" {
		if err := writeGraphJSON(g, opts.JSONPath); err != nil {
			return err
		}
	}
	if opts.Print {
		return g.WritePlan(stdout)
	}
	if observer.BeforeSchedule != nil {
		if err := observer.BeforeSchedule(ctx); err != nil {
			return err
		}
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
	log.Info("integration worktree", "dir", intDir, "branch", branch, "base", baseName)
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
	log.Info("connecting to herdr-core")
	runner, err := worker.Dial(ctx, cfg.HcBinary, cfg.HcSocket)
	if err != nil {
		return err
	}
	closeRunner := true
	defer func() {
		if closeRunner {
			_ = runner.Close()
		}
	}()
	log.Info("integration ready", "branch", branch, "mr", mrURL)
	log.Info("scheduler started", "parallelism", cfg.Parallelism, "agent", agentArgv)
	sched := scheduler.New(cfg, repo, fc, forge, g, runner, tmpl, workDir, intDir, branch, baseName, agentArgv, log)
	if observer.SchedulerReady != nil {
		// 调度尚未启动：GUI 此时才能拿到 scheduler，并且在第一个 worker
		// 起来之前就装上自己的刷新循环与终端连接方式。
		observer.SchedulerReady(sched, viewerAttach(cfg.HcBinary, cfg.HcSocket))
	}
	runErr := sched.Run(ctx)
	if sched.RetentionPending() {
		// delayed terminal 的清理定时器还要使用这条 hc 控制连接；Run 返回后
		// 由后台 goroutine 等最后一次清理结束再关闭，不阻塞 GUI/CLI 退出。
		closeRunner = false
		go func() {
			sched.WaitRetention()
			_ = runner.Close()
		}()
	}
	return runErr
}

// viewerAttach 返回用与调度同源的 hc 配置打开观看流的方式。每次
// attach 自己开一条连接，与调度器的 Runner 生命周期无关：关掉界面上的
// 终端不会影响 worker，后台连接关闭也不会连坐。连接由流自己持有，
// 关闭流时一并释放。
func viewerAttach(hcBinary, hcSocket string) AttachTerminal {
	return func(ctx context.Context, addr string, cols, rows int) (io.ReadWriteCloser, error) {
		return worker.OpenViewer(ctx, worker.ViewerOptions{
			Binary: hcBinary,
			Socket: hcSocket,
			Addr:   addr,
			Cols:   cols,
			Rows:   rows,
		})
	}
}

// initFiles 把配置与模板文件生成到 work-dir：优先复制 exe 目录
// 的同名文件（用户可能已在全局层定制过），exe 目录没有则写内置
// 默认内容。已存在的文件不覆盖，跳过并提示。
func initFiles(workDir string, out io.Writer) error {
	exeDir, err := files.ExeDir()
	if err != nil {
		return err
	}
	pairs := []struct{ name, fallback string }{
		{config.DefaultFile, config.DefaultConfig},
		{prompt.DefaultFile, prompt.DefaultTemplate},
	}
	for _, p := range pairs {
		dst := filepath.Join(workDir, p.name)
		data, err := os.ReadFile(filepath.Join(exeDir, p.name))
		if err != nil {
			data = []byte(p.fallback)
		}
		// 机器 ID 属于本机，-init 复制全局配置时不带进项目配置。
		if p.name == config.DefaultFile {
			data, err = config.StripInstanceID(data)
			if err != nil {
				return fmt.Errorf("prepare %s: %w", p.name, err)
			}
		}
		// O_EXCL 创建：并发 -init 或已存在的文件都不会被覆盖。
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				fmt.Fprintf(out, "%s already exists; skipped\n", dst)
				continue
			}
			return fmt.Errorf("write %s: %w", dst, err)
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if werr != nil {
			return fmt.Errorf("write %s: %w", dst, werr)
		}
		if cerr != nil {
			return fmt.Errorf("write %s: %w", dst, cerr)
		}
		fmt.Fprintf(out, "wrote %s\n", dst)
	}
	return nil
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
// 同时是调度器的 Forge 端口（Claim/Close/Note）。progress 非 nil 时
// 在建图过程中按阶段上报。
func buildGraph(ctx context.Context, kind, dir, host, projectPath string, specIID int, log *slog.Logger, progress func(graph.BuildProgress)) (forge.Forge, *graph.Graph, error) {
	switch kind {
	case config.ForgeGitLab:
		client, err := gitlab.NewClient(ctx, gitlab.ExecRunner{}, dir, host, projectPath)
		if err != nil {
			return nil, nil, err
		}
		g, err := graph.BuildWithProgress(ctx, glabSource{c: client, log: log}, specIID, progress)
		return client, g, err
	case config.ForgeGitHub:
		client, err := github.NewClient(ctx, github.ExecRunner{}, dir, host, projectPath)
		if err != nil {
			return nil, nil, err
		}
		g, err := graph.BuildWithProgress(ctx, ghSource{c: client, log: log}, specIID, progress)
		return client, g, err
	default:
		return nil, nil, fmt.Errorf("unknown forge %q", kind)
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
