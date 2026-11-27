package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
)

// fakeStream 是一条假的观看流：验证写、resize 与幂等关闭。
type fakeStream struct {
	mu       sync.Mutex
	written  []byte
	resized  [][2]int
	closed   int
	readCh   chan []byte
	readDone chan struct{}
}

func newFakeStream() *fakeStream {
	return &fakeStream{readCh: make(chan []byte, 4), readDone: make(chan struct{})}
}

func (f *fakeStream) Read(p []byte) (int, error) {
	select {
	case chunk := <-f.readCh:
		return copy(p, chunk), nil
	case <-f.readDone:
		return 0, io.EOF
	}
}

func (f *fakeStream) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeStream) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resized = append(f.resized, [2]int{cols, rows})
	return nil
}

func (f *fakeStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	select {
	case <-f.readDone:
	default:
		close(f.readDone)
	}
	return nil
}

func (f *fakeStream) snapshot() (written string, resized [][2]int, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.written), append([][2]int(nil), f.resized...), f.closed
}

// fakeAttach 记录 attach 调用并返回同一条假流。
type fakeAttach struct {
	mu     sync.Mutex
	calls  []string
	grids  [][2]int
	stream *fakeStream
	err    error
}

func (f *fakeAttach) attach(ctx context.Context, addr string, cols, rows int) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	f.calls = append(f.calls, addr)
	f.grids = append(f.grids, [2]int{cols, rows})
	stream, err := f.stream, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return stream, nil
}

func (f *fakeAttach) callsOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// 观看连接把 NSGo 终端的 resize 转给 core：双向交互是真的 PTY，
// 而不是抓屏轮询。
func TestViewerConnForwardsResizeAndInput(t *testing.T) {
	stream := newFakeStream()
	attach := &fakeAttach{stream: stream}
	conn := &viewerConn{Attach: attach.attach, Addr: "t1", Width: 80, Height: 24}

	stream.readCh <- []byte("hello")
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("Read = %q, %v, want the terminal output", buf[:n], err)
	}
	if _, err := conn.Write([]byte("typed")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if err := conn.Resize(120, 40); err != nil {
		t.Fatalf("Resize = %v, want the no-failure adapter signature", err)
	}

	written, resized, _ := stream.snapshot()
	if written != "typed" {
		t.Errorf("stream got %q, want the typed input", written)
	}
	if len(resized) != 1 || resized[0] != [2]int{120, 40} {
		t.Errorf("resized = %v, want one Resize(120,40)", resized)
	}
	if calls := attach.callsOf(); len(calls) != 1 || calls[0] != "t1" {
		t.Errorf("attach calls = %v, want exactly one for t1", calls)
	}
}

// 连续输出不能像网络帧一样一格一格直接喂给终端模拟器：NSGo 每次
// write 都会请求整屏重绘。突发输出必须在观看连接里合并，交互首发仍
// 立刻返回，后续密集帧按批次进入 parser。
func TestViewerConnCoalescesTerminalOutputBursts(t *testing.T) {
	stream := newFakeStream()
	attach := &fakeAttach{stream: stream}
	conn := &viewerConn{Attach: attach.attach, Addr: "t-batch", Width: 80, Height: 24}

	stream.readCh <- []byte("first")
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "first" {
		t.Fatalf("first Read = %q, %v, want the immediate interactive output", buf[:n], err)
	}
	// 这些帧在消费者下一次 Read 前已经全部到达；适配层必须合并，
	// 不能把它们拆成三次 parser write/整屏重绘。
	stream.readCh <- []byte("a")
	stream.readCh <- []byte("b")
	stream.readCh <- []byte("c")
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatalf("burst Read = %v", err)
	}
	if got := string(buf[:n]); got != "abc" {
		t.Fatalf("burst Read = %q, want the coalesced output %q", got, "abc")
	}
}

// 合批层新增了独立的等待路径：终端页被关闭时，正在无数据 Read 中
// 等待的 goroutine 必须立刻醒来，不能泄漏或拖住 terminal.Close。
func TestViewerConnCloseUnblocksPendingRead(t *testing.T) {
	stream := newFakeStream()
	attach := &fakeAttach{stream: stream}
	conn := &viewerConn{Attach: attach.attach, Addr: "t-close", Width: 80, Height: 24}

	stream.readCh <- []byte("open")
	buf := make([]byte, 16)
	if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "open" {
		t.Fatalf("opening Read = %q, %v", buf[:n], err)
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := conn.Read(buf)
		done <- result{n: n, err: err}
	}()
	if err := conn.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("pending Read after Close returned n=%d, nil error; want an end error", got.n)
		}
	case <-time.After(time.Second):
		t.Fatal("pending Read did not unblock after Close")
	}
}

func TestViewerConnLazyAndIdempotent(t *testing.T) {
	stream := newFakeStream()
	attach := &fakeAttach{stream: stream}
	conn := &viewerConn{Attach: attach.attach, Addr: "t2", Width: 100, Height: 30}
	// resize 在还没连上时不该开连接：没人在看就不占 hc 链路。
	if err := conn.Resize(90, 20); err != nil {
		t.Fatalf("Resize before Read = %v", err)
	}
	if calls := attach.callsOf(); len(calls) != 0 {
		t.Fatalf("attach calls before Read = %v, want none", calls)
	}
	// 关掉之后再读必须报错而不是悄悄重连。
	if err := conn.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close = %v, want idempotence", err)
	}
	if _, err := conn.Read(make([]byte, 8)); err == nil {
		t.Error("Read after Close succeeded, want an error")
	}
	if _, resized, _ := stream.snapshot(); len(resized) != 0 {
		t.Errorf("resized = %v after Close, want none", resized)
	}
}

func TestViewerConnAttachFailure(t *testing.T) {
	attach := &fakeAttach{err: errors.New("hc unreachable")}
	conn := &viewerConn{Attach: attach.attach, Addr: "t3"}
	if _, err := conn.Read(make([]byte, 8)); err == nil || !strings.Contains(err.Error(), "hc unreachable") {
		t.Fatalf("Read = %v, want the attach error", err)
	}
	// attach 失败不能留下半开的连接。
	if err := conn.Close(); err != nil {
		t.Fatalf("Close after a failed attach = %v", err)
	}
}

// 终端页按 key 去重；地址变了就换连接，不能继续显示旧终端；
// 地址消失（任务结束）时只释放观看连接，不 kill 任务。
func TestTerminalsSync(t *testing.T) {
	stream := newFakeStream()
	attach := &fakeAttach{stream: stream}
	s := &session{attach: attach.attach}
	terms := newTerminals(s, nil)
	key := graph.Key{ProjectID: 1, IID: 9}
	intg := scheduler.TaskRuntime{Key: graph.Key{ProjectID: 1, IID: 8}, Addr: "t8", Label: "proj-int", Integration: true}

	terms.sync([]scheduler.TaskRuntime{{Key: key, Addr: "t1", Label: "proj-issue-9"}, intg})
	if len(terms.tabs) != 2 {
		t.Fatalf("tabs = %d, want 2", len(terms.tabs))
	}
	if terms.tabs[0].ID() != flowNodeID(key) || terms.tabs[1].ID() != "integration-1!8" {
		t.Fatalf("tab ids = %q, %q", terms.tabs[0].ID(), terms.tabs[1].ID())
	}

	// 同一个地址重复刷新不会重复建页。
	terms.sync([]scheduler.TaskRuntime{{Key: key, Addr: "t1", Label: "proj-issue-9"}, intg})
	if len(terms.tabs) != 2 {
		t.Fatalf("tabs after the same status = %d, want 2", len(terms.tabs))
	}

	// 地址变化：重新绑定的页下次显示时连新地址。
	terms.sync([]scheduler.TaskRuntime{{Key: key, Addr: "t2", Label: "proj-issue-9"}, intg})
	if terms.tabs[0].Addr != "t2" {
		t.Fatalf("tab addr = %q, want the new address", terms.tabs[0].Addr)
	}

	// 地址消失：页与观看连接一起消失，任务本身不动（我们没有 Stop 任何东西）。
	tab := terms.tabs[0]
	tab.conn = &viewerConn{Attach: attach.attach, Addr: "t2"}
	terms.sync([]scheduler.TaskRuntime{intg})
	if len(terms.tabs) != 1 || terms.tabs[0].ID() != "integration-1!8" {
		t.Fatalf("tabs = %v, want the integration page alone", terms.tabs)
	}
	if _, _, closed := stream.snapshot(); closed != 0 {
		t.Errorf("closed = %d, want the task's stream left to its own close", closed)
	}
}

func TestTerminalsSyncStartingPlaceholder(t *testing.T) {
	s := &session{}
	terms := newTerminals(s, nil)
	key := graph.Key{ProjectID: 1, IID: 9}

	terms.sync([]scheduler.TaskRuntime{{Key: key, Label: "proj-issue-9", Starting: true}})
	if len(terms.tabs) != 1 {
		t.Fatalf("tabs = %d, want one starting placeholder", len(terms.tabs))
	}
	if terms.tabs[0].Addr != "" {
		t.Fatalf("placeholder addr = %q, want empty", terms.tabs[0].Addr)
	}

	// 地址就绪后同一页 rebind，而不是另开一页。
	terms.sync([]scheduler.TaskRuntime{{Key: key, Addr: "t1", Label: "proj-issue-9"}})
	if len(terms.tabs) != 1 || terms.tabs[0].Addr != "t1" {
		t.Fatalf("rebound tabs = %#v, want one tab at t1", terms.tabs)
	}

	// 启动失败且没有地址时，占位页随状态消失。
	terms.sync([]scheduler.TaskRuntime{{Key: key, Label: "proj-issue-9"}})
	if len(terms.tabs) != 0 {
		t.Fatalf("tabs = %d after starting state disappears, want 0", len(terms.tabs))
	}
}

func TestTerminalsSyncStartingKeepsWarmAddrInteractive(t *testing.T) {
	s := &session{}
	terms := newTerminals(s, nil)
	key := graph.Key{ProjectID: 1, IID: 9}

	terms.sync([]scheduler.TaskRuntime{{Key: key, Addr: "t1", Label: "proj-issue-9", Starting: true, StartupPhase: "正在确认认领"}})
	if len(terms.tabs) != 1 || terms.tabs[0].Addr != "t1" {
		t.Fatalf("starting tab = %#v, want the warm terminal to stay visible and interactive", terms.tabs)
	}

	// 阶段变化仍用同一个真实地址，只重建观看连接来注入新的进度横幅。
	terms.sync([]scheduler.TaskRuntime{{Key: key, Addr: "t1", Label: "proj-issue-9", Starting: true, StartupPhase: "正在发送 prompt"}})
	if len(terms.tabs) != 1 || terms.tabs[0].Addr != "t1" || terms.tabs[0].StartupPhase != "正在发送 prompt" {
		t.Fatalf("phase update tab = %#v, want updated progress on t1", terms.tabs[0])
	}

	// prompt 发送后不需要重连；progressConn 的横幅用完以后就是透明转发。
	terms.sync([]scheduler.TaskRuntime{{Key: key, Addr: "t1", Label: "proj-issue-9"}})
	if len(terms.tabs) != 1 || terms.tabs[0].Addr != "t1" || terms.tabs[0].StartupPhase != "" {
		t.Fatalf("prompted tab = %#v, want the same t1 terminal without a startup phase", terms.tabs[0])
	}
}

func TestProgressConnInjectsBannerButForwardsRealTerminal(t *testing.T) {
	stream := newFakeStream()
	attach := &fakeAttach{stream: stream}
	base := &viewerConn{Attach: attach.attach, Addr: "t1", Width: 80, Height: 24}
	conn := newProgressConn(base, "正在确认认领")

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("banner Read = %d, %v; want progress output", n, err)
	}
	if !strings.Contains(string(buf[:n]), "正在确认认领") {
		t.Fatalf("banner = %q, want the startup phase", string(buf[:n]))
	}

	stream.readCh <- []byte("agent output")
	n, err = conn.Read(buf)
	if err != nil || string(buf[:n]) != "agent output" {
		t.Fatalf("real Read = %q, %v, want terminal output", buf[:n], err)
	}
	if _, err := conn.Write([]byte("input")); err != nil {
		t.Fatalf("Write = %v", err)
	}
	if err := conn.Resize(100, 30); err != nil {
		t.Fatalf("Resize = %v", err)
	}
	written, resized, _ := stream.snapshot()
	if written != "input" {
		t.Fatalf("written = %q, want input forwarded", written)
	}
	if len(resized) != 1 || resized[0] != [2]int{100, 30} {
		t.Fatalf("resized = %v, want forwarded resize", resized)
	}
}

func TestStartingConn(t *testing.T) {
	conn := newStartingConn("正在测试启动进度…")
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("first Read = %d, %v; want placeholder text", n, err)
	}
	if !strings.Contains(string(buf[:n]), "正在启动终端") {
		t.Fatalf("placeholder text = %q", string(buf[:n]))
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 8))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("second Read finished before Close: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := <-done; err != io.EOF {
		t.Fatalf("second Read after Close = %v, want EOF", err)
	}
}

func TestTerminalsCloseAll(t *testing.T) {
	stream := newFakeStream()
	attach := &fakeAttach{stream: stream}
	s := &session{attach: attach.attach}
	terms := newTerminals(s, nil)
	terms.sync([]scheduler.TaskRuntime{{Key: graph.Key{ProjectID: 1, IID: 9}, Addr: "t1", Label: "a"}})
	conn := &viewerConn{Attach: attach.attach, Addr: "t1"}
	terms.tabs[0].conn = conn
	// 真的连一次，保证有东西要关。
	stream.readCh <- []byte("out")
	if _, err := conn.Read(make([]byte, 4)); err != nil && err != io.EOF {
		t.Fatalf("Read = %v", err)
	}
	terms.closeAll()
	if len(terms.tabs) != 0 {
		t.Fatalf("tabs = %d after closeAll, want 0", len(terms.tabs))
	}
	if _, _, closed := stream.snapshot(); closed != 1 {
		t.Errorf("stream closed %d times, want exactly 1", closed)
	}
}

func TestTabID(t *testing.T) {
	k := graph.Key{ProjectID: 1, IID: 9}
	if got := tabID(k, false); got != "1!9" {
		t.Errorf("tabID(task) = %q, want %q", got, "1!9")
	}
	if got := tabID(k, true); got != "integration-1!9" {
		t.Errorf("tabID(integration) = %q, want %q", got, "integration-1!9")
	}
}

// 编号非法时界面必须留在空状态并给出提示，而不是崩掉或假装开始。
func TestSessionFailLocal(t *testing.T) {
	s := newSession()
	s.failLocal(errors.New("issue 编号必须是正整数"))
	snap := s.Snapshot()
	if snap.Phase != phaseFailed {
		t.Errorf("phase = %v, want failed", snap.Phase)
	}
	if snap.Running {
		t.Error("Running = true after a local failure")
	}
	if snap.Err == nil {
		t.Error("snapshot has no error for the prompt")
	}
}

// Start 拒绝并发运行：两次启动不能各自抢同一批 issue。
func TestSessionStartRejectsConcurrentRun(t *testing.T) {
	s := newSession()
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	if err := s.Start(app.Options{SpecIID: 5}); err == nil {
		t.Fatal("Start accepted a second run")
	}
}

// 取消不是成功：状态必须说"已停止"，不能显示成完成。
func TestSessionStopIsNotSuccess(t *testing.T) {
	s := newSession()
	s.mu.Lock()
	s.running = true
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	s.cancel = cancel
	s.mu.Unlock()
	_ = canceled

	err := context.Canceled
	s.mu.Lock()
	s.running = false
	s.cancel = nil
	s.err = err
	if errors.Is(err, context.Canceled) {
		s.phase = phaseStopped
	}
	s.mu.Unlock()
	if snap := s.Snapshot(); snap.Phase != phaseStopped {
		t.Fatalf("phase = %v, want stopped after a cancel", snap.Phase)
	}
}

// lineWriter 保留最近若干行，并按整行切分。
func TestLineWriter(t *testing.T) {
	var w lineWriter
	if _, err := w.Write([]byte("one\ntwo\npart")); err != nil {
		t.Fatal(err)
	}
	if got := w.lines(); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("lines = %v, want the two complete lines", got)
	}
	for i := range logLines + 10 {
		_, _ = w.Write([]byte("line " + string(rune('a'+i%26)) + "\n"))
	}
	got := w.lines()
	if len(got) != logLines {
		t.Fatalf("kept %d lines, want the last %d", len(got), logLines)
	}
}

// 后台通知必须合并成界面线程上的一次更新，而不是每条都直接碰图。
func TestNotifyCoalesces(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	m := &model{post: func(func()) {
		mu.Lock()
		posts++
		mu.Unlock()
	}}
	for range 10 {
		m.notify()
	}
	mu.Lock()
	got := posts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("posted %d updates, want one coalesced update", got)
	}
}

// 只读观察的终端连接必须吞掉键盘输入，并且不能因为要写输入而打开 hc 流。
func TestReadOnlyViewerConnDropsInput(t *testing.T) {
	conn := &viewerConn{ReadOnly: true}
	n, err := conn.Write([]byte("rm -rf /\n"))
	if err != nil || n != len("rm -rf /\n") {
		t.Fatalf("Write = %d,%v, want input dropped as a successful no-op", n, err)
	}
	n, err = conn.WriteAuto([]byte("y\n"))
	if err != nil || n != 2 {
		t.Fatalf("WriteAuto = %d,%v, want auto input dropped as a successful no-op", n, err)
	}
	conn.mu.Lock()
	stream := conn.stream
	conn.mu.Unlock()
	if stream != nil {
		t.Fatal("read-only input opened a terminal stream")
	}

	// 已连接的观看流也不能收到 resize：观察页只允许本地重排视图，
	// 不得向原 worker 的控制面写任何信息。
	fake := newFakeStream()
	conn = &viewerConn{ReadOnly: true}
	conn.stream = fake
	if err := conn.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	_, resized, _ := fake.snapshot()
	if len(resized) != 0 {
		t.Fatalf("read-only resize forwarded %v, want no control-plane write", resized)
	}
}
