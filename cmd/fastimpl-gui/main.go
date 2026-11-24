package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nsgo-dev/nsgo"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/desktop"
	"github.com/yangjuncode/fastimpl/app"
)

func main() {
	opts, err := parseArguments(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h：用法已经打印，按约定以 0 退出。
			return
		}
		fmt.Fprintln(os.Stderr, "fastimpl-gui:", err)
		os.Exit(2)
	}
	os.Exit(runGUI(opts))
}

// runGUI 打开主窗口并等它结束。传入 issue 时窗口启动后自动开始构图与
// 调度；没传时以空状态打开，由用户在界面里录入。
//
// 窗口关闭、Ctrl+C 或 SIGTERM 都会取消运行，并给调度器时间收尾
// （停终端、释放认领与本地锁）。
func runGUI(opts app.Options) int {
	own := func(cx *core.Scope) core.Node {
		desktop.SetShutdownMode(cx, desktop.ShutdownOnMainWindowClose)
		s := newSession()
		m := newModel(cx, s, opts)
		// 关闭窗口先取消运行：退出请求在窗口真正消失之前处理，调度器
		// 的收尾因此还有机会执行；收尾最多等 quitGrace。
		desktop.OnQuitRequest(cx, func(q *desktop.QuitRequest) {
			if !s.Running() {
				return
			}
			s.Stop()
			quit := q.Defer()
			go func() {
				deadline := time.Now().Add(quitGrace)
				for s.Running() && time.Now().Before(deadline) {
					time.Sleep(50 * time.Millisecond)
				}
				quit(true)
			}()
		})
		signals(cx, s)
		if opts.SpecIID > 0 {
			// 传入 issue：启动后在后台构图并调度，窗口先出来。
			cx.Post(func() { m.start() })
		}
		return m.build()
	}
	nsgo.Run(own, nsgo.WindowSize(1180, 760))
	return 0
}

// quitGrace 是关闭窗口时等待调度器收尾的上限。
const quitGrace = 8 * time.Second

// signals 把 Ctrl+C/SIGTERM 变成取消运行，再退出进程。
func signals(cx *core.Scope, s *session) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	cx.Cleanup(func() { signal.Stop(ch) })
	go func() {
		if _, ok := <-ch; !ok {
			return
		}
		// 先取消，让调度器开始收尾，再请引擎退出；引擎退出前会等
		// 退出请求的处理，收尾因此不会被一刀切掉。
		s.Stop()
		cx.Post(func() { cx.Quit() })
	}()
}
