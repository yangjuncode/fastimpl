// Command fastimpl-gui 是 fastimpl 的 NSGO 图形界面版本，规划为每个进行
// 中的 herdr-core 会话对应一个 nsgo terminal tab，运行中可直接与 agent
// 对话。当前仅占位，先完成 cmd/fastimpl 命令行版本。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fastimpl-gui: GUI 版本尚未实现，请使用 cmd/fastimpl")
	os.Exit(1)
}
