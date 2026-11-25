// Command fastimpl 按 GitLab spec issue 的子任务阻塞关系分批并行运行
// Devin worker：每个子任务在自己的 worktree 中工作，完成后由 fastimpl
// 串行合入集成分支、推送并关闭 issue。
package main

import (
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
const usageLine = "usage: fastimpl [-version] [-init] [-print] [-json file] [-j N] [-config path] [-template path] [-repo-dir dir] [-tree-dir dir] [-work-dir dir] <spec-issue-iid>"

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
	// Ctrl+C/SIGTERM 取消运行：调度器 defer 的清理（如回收集成终端）
	// 才有机会执行，而不是进程直接被杀。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, opts, stdout, stderr)
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
	if flags.NArg() != 1 {
		flags.Usage()
		return app.Options{}, false, errors.New("expected exactly one spec issue IID")
	}
	iid, err := strconv.Atoi(strings.TrimSpace(flags.Arg(0)))
	if err != nil || iid <= 0 {
		return app.Options{}, false, fmt.Errorf("spec issue IID must be a positive integer, got %q", flags.Arg(0))
	}
	opts.SpecIID = iid
	return opts, false, nil
}
