package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/nsgo-dev/nsgo/base/terminal"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// termTab 是一个任务终端页：它按调度器公开的地址懒连一次，之后跨
// 页切换复用同一个终端对象，所以已完成任务的最后画面留在原处。
type termTab struct {
	// Key 是终端页的身份：任务用任务 key，集成分支 agent 用 spec 的
	// key 加 integration 标记。
	Key  graph.Key
	Intg bool
	// Addr 是这一页最近一次连上的地址；空表示还没连上。
	Addr string
	// Label 是终端名，供页标题与人工 hc attach。
	Label string
	term  *terminal.Terminal
	conn  *viewerConn
	// err 是这一页的可见错误：连不上、断开或结束。
	err error
}

// ID 返回终端页的稳定标识，与 flow 节点 ID 同源。
func (t *termTab) ID() string { return tabID(t.Key, t.Intg) }

// tabID 把任务身份映射成稳定的页标识。它与 flow 的节点 ID 规则一致，
// 因此点图上的节点就能跳到对应的终端。
func tabID(k graph.Key, integration bool) string {
	if integration {
		return "integration-" + k.String()
	}
	return k.String()
}

// viewerConn 是 terminal.Options.Conn 的适配层：把 worker.Viewer 的
// Resize(cols, rows) error 直接暴露给 NSGo 终端（它要求这个签名），
// 并保证 Close 幂等、关闭后不再写。
type viewerConn struct {
	// Attach 打开一次观看流；GUI 一定带它。
	Attach app.AttachTerminal
	Addr   string
	// Width、Height 是打开时的初始网格；terminal.View 随后会按自己的
	// 尺寸再 resize。
	Width, Height int

	mu     sync.Mutex
	stream interface {
		Read([]byte) (int, error)
		Write([]byte) (int, error)
		Close() error
	}
	closed bool
}

// Read 延迟到第一次 Read 才打开连接：这样终端页在还没被看过时不会
// 占一条 hc 链路，而 terminal 的读循环一旦跑起来就总能读到首屏。
func (c *viewerConn) Read(p []byte) (int, error) {
	stream, err := c.open()
	if err != nil {
		return 0, err
	}
	return stream.Read(p)
}

// open 打开（或复用）观看流。
func (c *viewerConn) open() (interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("terminal connection closed")
	}
	if c.stream != nil {
		return c.stream, nil
	}
	if c.Attach == nil {
		return nil, fmt.Errorf("terminal attach is not configured")
	}
	if c.Addr == "" {
		return nil, fmt.Errorf("terminal address is unknown")
	}
	cols, rows := c.Width, c.Height
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	stream, err := c.Attach(context.Background(), c.Addr, cols, rows)
	if err != nil {
		return nil, err
	}
	c.stream = stream
	return stream, nil
}

// Write 把按键送进终端；还没连上时先连。
func (c *viewerConn) Write(p []byte) (int, error) {
	stream, err := c.open()
	if err != nil {
		return 0, err
	}
	return stream.Write(p)
}

// Resize 把网格变化转给 core，程序因此收到 SIGWINCH。签名里的 error
// 是 NSGo 终端要求的；观看连接的 resize 没有可报的失败，失败已在
// 流结束时由 Read 报出。
func (c *viewerConn) Resize(cols, rows int) error {
	c.mu.Lock()
	stream, closed := c.stream, c.closed
	c.Width, c.Height = cols, rows
	c.mu.Unlock()
	if closed || stream == nil {
		return nil
	}
	if resizer, ok := stream.(interface{ Resize(int, int) error }); ok {
		return resizer.Resize(cols, rows)
	}
	return nil
}

// WriteAuto 送程序自动应答，不算人工输入；流不支持时退回 Write。
func (c *viewerConn) WriteAuto(p []byte) (int, error) {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()
	if auto, ok := stream.(interface{ WriteAuto([]byte) (int, error) }); ok {
		return auto.WriteAuto(p)
	}
	return c.Write(p)
}

// Close 关闭观看流；可重复调用。它保留流本身，好让 EndError 之后
// 仍说得出是怎么结束的。
func (c *viewerConn) Close() error {
	c.mu.Lock()
	stream, closed := c.stream, c.closed
	c.closed = true
	c.mu.Unlock()
	if closed || stream == nil {
		return nil
	}
	return stream.Close()
}

// EndError 报告流结束的原因：io.EOF 表示程序确认退出，其他非 nil
// 值表示断开（core 掉线、流被打断）。流仍在时返回 nil。
func (c *viewerConn) EndError() error {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()
	if ender, ok := stream.(interface{ EndError() error }); ok {
		return ender.EndError()
	}
	return nil
}

// terminals 管理终端页与它们的终端对象。它只在界面线程上被调用，
// 因此不需要加锁；hc 的连接与关闭都在界面线程之外发生。
type terminals struct {
	session *session
	tabs    []*termTab
	byID    map[string]*termTab
	active  int
	// notify 在终端自己报出结束/断开后请界面刷新一次：错误必须
	// 可见，而且能一键重连。
	notify func()
}

// newTerminals 创建空的终端集合。
func newTerminals(s *session, notify func()) *terminals {
	return &terminals{session: s, byID: map[string]*termTab{}, notify: notify}
}

// sync 按调度器的运行状态同步终端页：每个有地址的条目一页，按 key
// 去重。它不主动建连接，连接在终端页第一次被显示时懒建立。
// 离开的运行（地址消失）会释放自己的观看连接，但绝不 kill 任务。
func (t *terminals) sync(status []scheduler.TaskRuntime) {
	live := make(map[string]scheduler.TaskRuntime, len(status))
	for _, r := range status {
		if r.Addr == "" {
			continue
		}
		live[tabID(r.Key, r.Integration)] = r
	}
	// 先删掉这一轮已经不存在的页。
	kept := t.tabs[:0]
	for _, tab := range t.tabs {
		if _, ok := live[tab.ID()]; ok {
			kept = append(kept, tab)
			continue
		}
		delete(t.byID, tab.ID())
		tab.close()
	}
	t.tabs = kept
	// 再按调度器的顺序补上新页，并更新已有页的地址：地址变了就是
	// 另一个终端，必须重连，不能继续显示旧的流。
	for _, r := range status {
		if r.Addr == "" {
			continue
		}
		id := tabID(r.Key, r.Integration)
		if tab, ok := t.byID[id]; ok {
			if tab.Addr != r.Addr {
				tab.rebind(r.Addr, r.Label)
			}
			continue
		}
		tab := &termTab{Key: r.Key, Intg: r.Integration, Addr: r.Addr, Label: r.Label}
		t.tabs = append(t.tabs, tab)
		t.byID[id] = tab
	}
	if t.active >= len(t.tabs) {
		t.active = max(len(t.tabs)-1, 0)
	}
}

// closeAll 释放全部观看连接（窗口关闭或运行结束时）。
func (t *terminals) closeAll() {
	for _, tab := range t.tabs {
		tab.close()
	}
	t.tabs, t.byID = nil, map[string]*termTab{}
	t.active = 0
}

// close 释放这一页的观看连接并停掉终端；任务是 core 上的另一个进程，
// 不受影响。
func (t *termTab) close() {
	if t.term != nil {
		_ = t.term.Close()
		t.term = nil
	}
	if t.conn != nil {
		_ = t.conn.Close()
		t.conn = nil
	}
}

// rebind 把页改连到新地址：旧流作废，新地址下次显示时重连。
func (t *termTab) rebind(addr, label string) {
	t.close()
	t.Addr, t.Label, t.err = addr, label, nil
}

// retry 丢掉当前终端，让下一次显示重连：终端页断流或连不上时用。
func (t *termTab) retry() {
	t.close()
	t.err = nil
}

// ensure 保证这一页有终端对象，没有就建一个。它只在界面线程上跑；
// 建终端本身不做网络 IO，连接在 terminal 的读循环里懒建立。
func (t *terminals) ensure(cx *core.Scope, tab *termTab) *terminal.Terminal {
	if tab.term != nil {
		return tab.term
	}
	conn := &viewerConn{
		Attach: t.session.attach,
		Addr:   tab.Addr,
	}
	term, err := terminal.New(terminal.Options{
		Conn: conn,
		// 不设 ResizeByConn：那个开关让本地屏幕保持远端尺寸、只由
		// Conn 的读者改尺寸。这里要的是相反的方向——视图量出网格后
		// 本地模拟器按它排版，同时经 Conn 的 Resize(cols, rows) error
		// 告诉 core，程序因此收到 SIGWINCH。交互是真的双向 PTY。
		CursorBlink: true,
		OnExit: func(code int) {
			cx.Post(func() {
				if tab.term == nil {
					return
				}
				if end := conn.EndError(); end != nil && !errors.Is(end, io.EOF) {
					tab.err = fmt.Errorf("终端已断开：%w", end)
				} else {
					tab.err = fmt.Errorf("终端已结束（退出码 %d）", code)
				}
				if t.notify != nil {
					t.notify()
				}
			})
		},
	})
	if err != nil {
		tab.err = err
		_ = conn.Close()
		return nil
	}
	tab.term, tab.conn = term, conn
	return term
}
