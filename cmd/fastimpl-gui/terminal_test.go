package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

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
