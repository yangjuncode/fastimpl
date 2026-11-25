// Command fastimpl-gui 是 fastimpl 的桌面界面：选 issue、看依赖图、
// 跟随构图进度与调度状态，并在每个 worker 的终端页里直接与 agent
// 对话。它复用 cmd/fastimpl 的同一套运行管线（app.RunObserved），
// 依赖图建好后先停在确认门禁前，用户点击开始调度后才连接 hcgo。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/yangjuncode/fastimpl/app"
)

// usageLine 是命令行用法说明。
const usageLine = "usage: fastimpl-gui [-j N] [-config path] [-template path] [-repo-dir dir] [-tree-dir dir] [-work-dir dir] [spec-issue-iid]"

// parseArguments 解析命令行；位置参数是可选的 spec issue IID。
// 不给 IID 时主窗口以空状态打开，用户在界面里录入。
func parseArguments(args []string, stderr io.Writer) (app.Options, error) {
	flags := flag.NewFlagSet("fastimpl-gui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts app.Options
	flags.IntVar(&opts.Parallelism, "j", 0, "maximum concurrent workers (0: use config or default)")
	flags.StringVar(&opts.ConfigPath, "config", "", "path to fastimpl.toml (default: <work-dir>/fastimpl.toml, fallback: <exe-dir>/fastimpl.toml)")
	flags.StringVar(&opts.TemplatePath, "template", "", "worker prompt template (default: <work-dir>/fastimpl.prompt.md, fallback: <exe-dir>/fastimpl.prompt.md)")
	flags.StringVar(&opts.RepoDir, "repo-dir", "", "target repository main checkout (must be the repository's top level; default: locate within 8 levels up/down from work-dir)")
	flags.StringVar(&opts.TreeDir, "tree-dir", "", "worktree parent directory (default: infer from existing worktrees, else <repo-dir>/.worktrees)")
	flags.StringVar(&opts.WorkDir, "work-dir", "", "fastimpl working directory (default: current directory)")
	flags.Usage = func() {
		_, _ = io.WriteString(stderr, usageLine+"\n")
		flags.PrintDefaults()
	}
	// -h 走 flag 自己的 ContinueOnError：它打印用法并返回
	// flag.ErrHelp，调用方据此以 0 退出，而不是当成错误。
	if err := flags.Parse(args); err != nil {
		return app.Options{}, err
	}
	if opts.Parallelism < 0 {
		return app.Options{}, fmt.Errorf("-j must not be negative, got %d", opts.Parallelism)
	}
	switch flags.NArg() {
	case 0:
		return opts, nil
	case 1:
		raw := strings.TrimSpace(flags.Arg(0))
		iid, err := strconv.Atoi(raw)
		if err != nil || iid <= 0 {
			return app.Options{}, fmt.Errorf("spec issue IID must be a positive integer, got %q", flags.Arg(0))
		}
		opts.SpecIID = iid
		return opts, nil
	default:
		flags.Usage()
		return app.Options{}, errors.New("expected at most one spec issue IID")
	}
}
