package retry

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTransient(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("glab api x: exit status 1: ERROR Get \"https://h/api\": dial tcp 1.2.3.4:443: i/o timeout"), true},
		{errors.New("gh api: TLS handshake timeout"), true},
		{errors.New("connect: connection refused"), true},
		{errors.New("read: connection reset by peer"), true},
		{errors.New("lookup h: no such host"), true},
		{errors.New("HTTP 503: Service Unavailable"), true},
		{errors.New("HTTP 500: Internal Server Error"), true},
		{errors.New("glab api: exit status 1: 500 Internal Server Error"), true},
		{errors.New("gh api: HTTP 408 Request Timeout"), true},
		{errors.New("Post https://gitlab.example.com/api: EOF"), true},
		{errors.New("context deadline exceeded"), true},
		{errors.New("unexpected status code: 500"), true},
		{errors.New("upstream response 599"), true},
		{errors.New("glab api: exit status 1: ERROR 404 Not Found"), false},
		{errors.New("HTTP 422: source_branch does not exist"), false},
		{errors.New("issue 3 not found"), false},
		{errors.New("issue #500 not found"), false},
		{errors.New("json: cannot decode"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := Transient(c.err); got != c.want {
			t.Errorf("Transient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// 重试进度必须能写入调用方提供的日志流；GUI 的日志页/log_file 不是
// 进程 stderr，靠 os.Stderr 会让长时间重试完全不可见。第一次失败后
// 立刻取消 ctx，避免测试真的等待退避。
func TestDoWriterWritesRetryProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var progress strings.Builder
	calls := 0
	_, err := DoWriter(ctx, "glab api test", &progress, func() ([]byte, error) {
		calls++
		cancel()
		return nil, errors.New("dial tcp: i/o timeout")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled after cancel during backoff", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 before cancellation", calls)
	}
	text := progress.String()
	if !strings.Contains(text, "glab api test failed") || !strings.Contains(text, "retrying") || !strings.Contains(text, "i/o timeout") {
		t.Fatalf("progress = %q, want the failed command, retry hint and cause", text)
	}
}
