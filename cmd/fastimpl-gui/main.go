package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/nsgo-dev/nsgo"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/version"
)

func main() {
	opts, showVersion, err := parseArguments(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h：用法已经打印，按约定以 0 退出。
			return
		}
		fmt.Fprintln(os.Stderr, "fastimpl-gui:", err)
		os.Exit(2)
	}
	if showVersion {
		fmt.Printf("fastimpl-gui version %s\n", version.String())
		return
	}
	os.Exit(runGUI(opts))
}

// runGUI 打开主窗口并等它结束。传入 issue 时窗口启动后自动构图，
// 但调度会停在确认门禁前；没传时以空状态打开，由用户在界面里录入。
//
// 窗口关闭采用 detach 语义：GUI 进程退出，但 hc 里的 worker 继续运行；
// active metadata/heartbeat 留在本机，下一次启动据此观察或接管。只有界面
// 上的“停止”按钮会取消调度并执行停终端、释放认领与本地锁的收尾。
// 即使进程收到 Ctrl+C/SIGTERM 也不偷偷取消 worker：命令行 Ctrl+C 的显式
// 中断语义只属于 CLI；GUI 的显式停止入口就是停止按钮。
func runGUI(opts app.Options) int {
	own := func(cx *core.Scope) core.Node {
		desktop.SetShutdownMode(cx, desktop.ShutdownOnMainWindowClose)
		s := newSession()
		m := newModel(cx, s, opts)
		if opts.SpecIID > 0 {
			// 传入 issue：启动后在后台构图，窗口先出来；调度仍需用户确认。
			cx.Post(func() { m.start() })
		}
		return m.build()
	}
	nsgo.Run(own, nsgo.WindowSize(1180, 760))
	return 0
}
