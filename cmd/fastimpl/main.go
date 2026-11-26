// Command fastimpl 按 GitLab spec issue 的子任务阻塞关系分批并行运行
// Devin worker：每个子任务在自己的 worktree 中工作，完成后由 fastimpl
// 串行合入集成分支、推送并关闭 issue。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/version"
)

// usageLine 是命令行用法说明。
const usageLine = "usage: fastimpl [-version] [-init] [-print] [-json file] [-j N] [-config path] [-template path] [-repo-dir dir] [-tree-dir dir] [-work-dir dir] [spec-issue-iid]"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fastimpl:", err)
		os.Exit(1)
	}
}

// run 解析命令行参数并交给 app.Run 执行。
func run(args []string, stdout, stderr io.Writer) error {
	opts, showVersion, err := parseArguments(args, stderr)
	if err != nil {
		return err
	}
	if showVersion {
		_, _ = fmt.Fprintf(stdout, "fastimpl version %s\n", version.String())
		return nil
	}
	// 版本属于诊断信息，写到 stderr，避免污染 -print/-json 的 stdout 数据。
	_, _ = fmt.Fprintf(stderr, "fastimpl version %s\n", version.String())
	// Ctrl+C/SIGTERM 不再直接取消 worker。信号处理器会询问是否停止；
	// 默认（包括非交互 stdin 无法询问）只退出 CLI 进程，hc 里的 worker
	// 继续运行，active metadata 留给下一次启动接管。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopWatching := watchInterrupts(cancel, stderr)
	defer stopWatching()
	return app.Run(ctx, opts, stdout, stderr)
}

func shouldStopWorkers(answer string) bool {
	answer = strings.TrimSpace(answer)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
}

// watchInterrupts 把 Ctrl+C/SIGTERM 变成一次显式确认。回答 y/yes 才
// cancel 调度上下文并走停止清理；其它回答、EOF 或非交互运行都 detach：
// 直接结束 CLI 进程，不 cancel，因此 fastimpl 的 goroutine 随进程消失，
// 但 hc worker 与 active metadata 留下。
func watchInterrupts(cancel context.CancelFunc, stderr io.Writer) func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
		case <-done:
			return
		}
		fmt.Fprintln(stderr, "\n收到中断。停止所有 fastimpl worker 并释放 claim/lock 吗？输入 y 确认；其它回答将只退出 CLI、让 worker 继续运行。")
		fmt.Fprint(stderr, "stop workers? [y/N] ")
		answer := ""
		if stat, err := os.Stdin.Stat(); err == nil && stat.Mode()&os.ModeCharDevice != 0 {
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			answer = strings.TrimSpace(line)
		}
		if shouldStopWorkers(answer) {
			fmt.Fprintln(stderr, "正在停止 worker 并清理 claim/lock…")
			cancel()
			return
		}
		fmt.Fprintln(stderr, "按默认选择 detach：CLI 退出，hc worker 继续运行；下次启动可用 active metadata 接管。")
		os.Exit(0)
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// parseArguments 解析命令行；位置参数是 spec issue 的 IID。
func parseArguments(args []string, stderr io.Writer) (app.Options, bool, error) {
	flags := flag.NewFlagSet("fastimpl", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts app.Options
	var showVersion bool
	flags.BoolVar(&showVersion, "version", false, "print the fastimpl version and exit")
	flags.BoolVar(&opts.Init, "init", false, "write fastimpl.toml and fastimpl.prompt.md into the work directory and exit")
	flags.BoolVar(&opts.Print, "print", false, "print the dependency graph and exit without writing anything")
	flags.StringVar(&opts.JSONPath, "json", "", "write the full dependency graph (with open/closed state) to this JSON file")
	flags.IntVar(&opts.Parallelism, "j", 0, "maximum concurrent workers (0: use config or default)")
	flags.StringVar(&opts.ConfigPath, "config", "", "path to fastimpl.toml (default: <work-dir>/fastimpl.toml, fallback: <exe-dir>/fastimpl.toml)")
	flags.StringVar(&opts.TemplatePath, "template", "", "worker prompt template (default: <work-dir>/fastimpl.prompt.md, fallback: <exe-dir>/fastimpl.prompt.md)")
	flags.StringVar(&opts.RepoDir, "repo-dir", "", "target repository main checkout (default: locate within 8 levels up/down from work-dir)")
	flags.StringVar(&opts.TreeDir, "tree-dir", "", "worktree parent directory (default: infer from existing worktrees, else <repo-dir>/.worktrees)")
	flags.StringVar(&opts.WorkDir, "work-dir", "", "fastimpl working directory (default: current directory)")
	flags.Usage = func() {
		_, _ = io.WriteString(stderr, usageLine+"\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return app.Options{}, false, err
	}
	if showVersion {
		return opts, true, nil
	}
	// -init 不需要 spec IID；给了反而说明用法有误。
	if opts.Init {
		if flags.NArg() != 0 {
			flags.Usage()
			return app.Options{}, false, errors.New("-init does not take a spec issue IID")
		}
		return opts, false, nil
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return app.Options{}, false, errors.New("expected at most one spec issue IID")
	}
	if flags.NArg() == 0 {
		// 无 spec：app 层扫描本仓库 active metadata，能安全接管时自动恢复；
		// 活 supervisor 只读报告，找不到可恢复现场时再报错。
		return opts, false, nil
	}
	iid, err := strconv.Atoi(strings.TrimSpace(flags.Arg(0)))
	if err != nil || iid <= 0 {
		return app.Options{}, false, fmt.Errorf("spec issue IID must be a positive integer, got %q", flags.Arg(0))
	}
	opts.SpecIID = iid
	return opts, false, nil
}
