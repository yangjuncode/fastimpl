// Package retry 重试子进程调用中的瞬时网络故障。glab/gh 内部 HTTP
// 客户端的 dial/TLS 超时不对外开放配置，弱网环境下唯一的手段是
// 退避重试；每次重试在 stderr 留一行进度，长跑调用不再静默。
package retry

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// Attempts 是一次调用的最大尝试次数（含首次）：弱网实例要求
// 近乎"重试到成功"，100 次 × 最长 30s 间隔兜底约一小时。
const Attempts = 100

// maxBackoff 封顶单次退避时长。
const maxBackoff = 30 * time.Second

// Do 执行 fn：Transient 错误退避重试；成功、非瞬时错误或尝试
// 耗尽时返回最后一次结果。ctx 取消立即返回，不再重试。重试进度写
// 到进程 stderr；需要并入调用方日志流时用 DoWriter。
func Do(ctx context.Context, label string, fn func() ([]byte, error)) ([]byte, error) {
	return DoWriter(ctx, label, os.Stderr, fn)
}

// DoWriter 同 Do，但把每次重试进度写入 progress。GUI 的 stderr 不是
// 进程 stderr，而是日志页/log_file 的 lineWriter；forge runner 必须把
// 这个流传到这里，否则长时间重试在 GUI 里完全不可见。progress 为空时
// 退回进程 stderr，保持 CLI/测试零值行为。
func DoWriter(ctx context.Context, label string, progress io.Writer, fn func() ([]byte, error)) ([]byte, error) {
	if progress == nil {
		progress = os.Stderr
	}
	for attempt := 1; ; attempt++ {
		out, err := fn()
		if err == nil || !Transient(err) || attempt >= Attempts {
			return out, err
		}
		delay := backoff(attempt)
		fmt.Fprintf(progress, "fastimpl: %s failed (attempt %d/%d), retrying in %s: %v\n",
			label, attempt, Attempts, delay, err)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

// backoff 指数退避：1s、2s、4s、8s，封顶 maxBackoff。
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

// Transient 报告错误是否值得重试：连接阶段的超时/拒绝/重置、
// DNS 抖动、上游 5xx 与限流——这些失败发生时请求多半未到达
// 服务器，重试写操作同样安全。匹配作用于 CLI 包装后的错误
// 文本（命令行 + 退出状态 + stderr 摘要）。
func Transient(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, pat := range transientPatterns {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return transientHTTPStatus.MatchString(s) || bareEOF.MatchString(s)
}

// transientHTTPStatus 识别 CLI 错误文本里的 408/429/5xx。5xx 与 429
// 都表示上游或边缘层本次请求失败；即便写操作存在“服务端已执行但响应
// 丢失”的理论风险，这与既有 502/503/504 重试语义一致，且总次数有界。
var transientHTTPStatus = regexp.MustCompile(`\b(?:http[ -]?(?:408|429|5[0-9][0-9])|(?:status|response|error)(?:\s+code)?\s*[:=]?\s*(?:408|429|5[0-9][0-9])|(?:408|429|5[0-9][0-9])\s+(?:request timeout|too many requests|internal server error|bad gateway|service unavailable|gateway timeout))\b`)

// bareEOF 识别 Go HTTP 传输层常见的裸 `EOF`/`unexpected EOF` 类响应
// 截断。 unexpected EOF 已由文本模式覆盖；这里补 `: EOF` 这种短文本。
var bareEOF = regexp.MustCompile(`\beof\b`)

// transientPatterns 是瞬时错误的文本特征（小写匹配）。
var transientPatterns = []string{
	"timeout", // i/o timeout、TLS handshake timeout
	"timed out",
	"deadline exceeded",
	"internal server error", // HTTP 500 的常见完整文本
	"connection refused",
	"connection reset",
	"unexpected eof",
	"no such host",
	"no route to host",
	"network is unreachable",
	"temporary failure",
	"server misbehaving",
	"bad gateway",         // HTTP 502
	"service unavailable", // HTTP 503
	"gateway timeout",     // HTTP 504
	"too many requests",   // HTTP 429
	"http 502", "http 503", "http 504", "http 429",
}
