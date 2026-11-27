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
	"github.com/yangjuncode/fastimpl/issuecontext"
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

// buildGraphFn 是可测试的接缝：生产环境始终使用 buildGraph，测试可以
// 替换它来把运行管线推进到调度门禁，而不访问真实 forge。
var buildGraphFn = buildGraph

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
	cfg, err := loadRunConfig(workDir, opts)
	if err != nil {
		return err
	}
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
	if opts.SpecIID == 0 {
		// CLI 未输入 spec 时先扫描本仓库 active metadata：死 supervisor 的
		// 可接管现场直接恢复原 spec；活 supervisor 只报告 hc attach 信息，
		// 绝不进入第二个 scheduler。
		spec, handled, err := recoverSpecFromActive(repoDir, stderr, log)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
		if spec == 0 {
			return errors.New("spec issue IID is required; no recoverable active fastimpl run was found for this repository")
		}
		opts.SpecIID = spec
		cfg.SpecIID = spec
	}
	repo := worktree.NewRepo(repoDir, "")
	host, projectPath, err := repo.OriginCoordinate(ctx)
	if err != nil {
		return repoError(repoDir, err)
	}
	forge := resolveForge(ctx, cfg.Forge, host, repoDir)
	log.Info("building dependency graph", "forge", forge, "origin", host+"/"+projectPath, "spec", opts.SpecIID)
	buildStart := time.Now()
	fc, g, contexts, err := buildGraphFn(ctx, forge, repoDir, host, projectPath, opts.SpecIID, log, observer.Progress, !opts.Print)
	if err != nil {
		return err
	}
	if contexts != nil {
		contextDir := issuecontext.Dir(path.Base(projectPath))
		written, writeErr := contexts.WriteAll(contextDir, g)
		switch {
		case writeErr != nil:
			// 这是启动加速快照，不是正确性前提；写失败时让 worker 走模板
			// 里的 forge 读取兜底，不能把整个调度卡死。
			log.Warn("write issue context snapshots failed", "dir", contextDir, "err", writeErr)
		default:
			log.Info("issue context snapshots written", "dir", contextDir, "files", written.Files)
		}
		if written.CommentErrors > 0 {
			log.Warn("some issue comments could not be prefetched", "count", written.CommentErrors)
		}
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
	// -print/-json 与建图都是只读观察，不拦截；一旦要越过 GUI/CLI 的
	// 调度门禁去准备集成分支、push/EnsureMR 或连接 hc，就必须先确认没有
	// live supervisor。否则即使任务级最后拒绝偷锁，前面的写操作已经违规。
	if err := checkActiveSpecBeforeWrites(repoDir, opts.SpecIID, time.Now()); err != nil {
		return err
	}
	if observer.BeforeSchedule != nil {
		if err := observer.BeforeSchedule(ctx); err != nil {
			return err
		}
	}
	// GUI 的确认门禁可能敞开很久；门禁前的 preflight 不能覆盖等待期间
	// 另一个实例刚刚启动的情况。真正写集成分支/连接 hc 前再 authoritative
	// 检查一次。
	if err := checkActiveSpecBeforeWrites(repoDir, opts.SpecIID, time.Now()); err != nil {
		return err
	}
	// GUI 的确认门禁可能敞开很久；等待期间用户在设置页保存的配置
	// 只落在磁盘上，门禁前那份 cfg 不会自动更新。真正准备集成、连接
	// hc 或组装 scheduler 前必须重读，确保开工使用的是最新生效配置。
	cfg, err = loadRunConfig(workDir, opts)
	if err != nil {
		return fmt.Errorf("reload config before scheduling: %w", err)
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
		fmt.Sprintf("spec #%d integration", opts.SpecIID),
		fmt.Sprintf("Close #%d", opts.SpecIID))
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
	sched.SetRuntimeSettingsLoader(runtimeSettingsLoader(workDir, opts))
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

// ViewerAttach 返回用给定 hc 配置打开观看流的方式。GUI 的正常运行与
// active worker 只读观察共用这条路径；每次 attach 自己开一条连接，与
// 调度器的 Runner 生命周期无关：关掉界面上的终端不会影响 worker，后台
// 连接关闭也不会连坐。连接由流自己持有，关闭流时一并释放。
func ViewerAttach(hcBinary, hcSocket string) AttachTerminal {
	return viewerAttach(hcBinary, hcSocket)
}

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

// loadRunConfig 读取一次运行当前生效的配置，并叠加命令行覆盖。
// 初次加载与调度门禁后的重读必须走同一个入口，否则 -j 或恢复出的
// spec IID 会在重读时丢失。
func loadRunConfig(workDir string, opts Options) (config.Config, error) {
	cfg, err := config.Load(workDir, opts.ConfigPath)
	if err != nil {
		return config.Config{}, err
	}
	if opts.Parallelism > 0 {
		cfg.Parallelism = opts.Parallelism
	}
	cfg.SpecIID = opts.SpecIID
	return cfg, nil
}

// runtimeSettingsLoader 给 scheduler 提供可安全热更新的任务设置。
// 每次调用都重新读 work-dir 配置与提示词，因此 GUI 设置页保存后，
// 后续新 worker、首次 prompt、续写重试与终端收尾会使用最新值。
// repo/forge/hc/instance/integration 不在快照里：它们已经在本轮的
// 构图、连接或恢复身份中消费，不能热切换。
func runtimeSettingsLoader(workDir string, opts Options) scheduler.RuntimeSettingsLoader {
	return func(context.Context) (scheduler.RuntimeSettings, error) {
		cfg, err := loadRunConfig(workDir, opts)
		if err != nil {
			return scheduler.RuntimeSettings{}, err
		}
		tmpl, err := prompt.Load(workDir, opts.TemplatePath)
		if err != nil {
			return scheduler.RuntimeSettings{}, err
		}
		agentArgv, err := cfg.AgentArgv()
		if err != nil {
			return scheduler.RuntimeSettings{}, err
		}
		return scheduler.RuntimeSettings{
			Parallelism:       cfg.Parallelism,
			Retries:           cfg.Retries,
			AgentArgv:         agentArgv,
			RequireReview:     cfg.RequireReview,
			Reviewer:          cfg.ReviewerName(),
			TerminalRetention: cfg.TerminalRetention,
			Template:          tmpl,
		}, nil
	}
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
func buildGraph(ctx context.Context, kind, dir, host, projectPath string, specIID int, log *slog.Logger, progress func(graph.BuildProgress), prefetch bool) (forge.Forge, *graph.Graph, *issuecontext.Store, error) {
	wrap := func(client forge.Forge, base graph.Source) (graph.Source, *issuecontext.Store) {
		if !prefetch {
			return base, nil
		}
		store := issuecontext.NewStore()
		return store.WrapSource(base, client), store
	}
	switch kind {
	case config.ForgeGitLab:
		client, err := gitlab.NewClient(ctx, gitlab.ExecRunner{}, dir, host, projectPath)
		if err != nil {
			return nil, nil, nil, err
		}
		src, store := wrap(client, glabSource{c: client, log: log})
		g, err := graph.BuildWithProgress(ctx, src, specIID, progress)
		return client, g, store, err
	case config.ForgeGitHub:
		client, err := github.NewClient(ctx, github.ExecRunner{}, dir, host, projectPath)
		if err != nil {
			return nil, nil, nil, err
		}
		src, store := wrap(client, ghSource{c: client, log: log})
		g, err := graph.BuildWithProgress(ctx, src, specIID, progress)
		return client, g, store, err
	default:
		return nil, nil, nil, fmt.Errorf("unknown forge %q", kind)
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
