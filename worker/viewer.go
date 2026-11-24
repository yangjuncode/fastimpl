package worker

import (
	"context"
	"errors"
	"io"
	"sync"

	hc "github.com/yangjuncode/hcgo"
)

// ViewerOptions 描述一次观看连接。
type ViewerOptions struct {
	// Binary 与 Socket 与 Dial 相同：Socket 非空直连 Unix socket，
	// 否则用 Binary（空时按 PATH 找 hc）执行 hc connect。
	Binary, Socket string
	// Addr 是终端地址，由 scheduler 公开。
	Addr string
	// Cols、Rows 是初始网格；terminal.View 之后会按自己的尺寸再
	// resize。
	Cols, Rows int
	// History 是首帧回放的 scrollback 行数；0 表示默认。
	History int
}

// Viewer 是一个独立的观看连接：自有一条到 herdr-core 的链路，
// 与调度用的 Runner 互不相干——关掉观看不会影响 worker 本身，
// 后台连接关闭也不会连坐。
//
// 它把两个无返回值的 hcgo 接口适配成 NSGo 终端要求的签名：
// Conn 需要 Resize(cols, rows int) error 才会收到网格变化，
// hcgo.View.Resize 不返回 error，这里补上。
type Viewer struct {
	client *hc.Client
	view   *hc.View
	// resizeMu 让 resize 与 close 不交错：Close 之后不再发帧。
	resizeMu sync.Mutex
	closed   bool
	closeOne sync.Once
}

// OpenViewer 用独立连接打开终端的观看流。调用方负责 Close。
func OpenViewer(ctx context.Context, o ViewerOptions) (*Viewer, error) {
	if o.Addr == "" {
		return nil, errors.New("open viewer: empty terminal address")
	}
	opts := hc.Options{Socket: o.Socket}
	if o.Socket == "" {
		opts.Binary = o.Binary
	}
	client, err := hc.Dial(ctx, opts)
	if err != nil {
		return nil, err
	}
	view, err := client.Attach(o.Addr, o.Cols, o.Rows, hc.AttachOptions{History: o.History, ResizeNotifications: true})
	if err != nil {
		client.Close()
		return nil, err
	}
	return &Viewer{client: client, view: view}, nil
}

// Read 读程序输出；首个 Read 是 attach 时的整屏画面。
func (v *Viewer) Read(p []byte) (int, error) { return v.view.Read(p) }

// Write 把按键送给程序，是真正的双向：写的就是终端的输入字节。
func (v *Viewer) Write(p []byte) (int, error) { return v.view.Write(p) }

// WriteAuto 送程序自动应答（终端查询回复）。core 未确认
// automatic_input 时 hcgo 自己退回普通 Write。
func (v *Viewer) WriteAuto(p []byte) (int, error) { return v.view.WriteAuto(p) }

// Resize 告诉 core 观看者的网格尺寸；core 会让程序收到 SIGWINCH。
// 这就是 NSGo 终端识别的那个无副作用签名。
func (v *Viewer) Resize(cols, rows int) error {
	v.resizeMu.Lock()
	defer v.resizeMu.Unlock()
	if v.closed {
		return nil
	}
	v.view.Resize(cols, rows)
	return nil
}

// EndError 报告观看流结束的原因；io.EOF 表示进程确认退出，
// 流仍在时返回 nil。
func (v *Viewer) EndError() error { return v.view.EndError() }

// Close 结束观看并关闭自有连接；程序继续在 core 上运行。可重复调用。
func (v *Viewer) Close() error {
	v.closeOne.Do(func() {
		v.resizeMu.Lock()
		v.closed = true
		v.resizeMu.Unlock()
		_ = v.view.Close()
		v.client.Close()
	})
	return nil
}

// 编译期确认 Viewer 满足 NSGo 终端对 Conn 的两种可选接口。
var (
	_ io.ReadWriteCloser                          = (*Viewer)(nil)
	_ interface{ Resize(int, int) error }         = (*Viewer)(nil)
	_ interface{ WriteAuto([]byte) (int, error) } = (*Viewer)(nil)
)
