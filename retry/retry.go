// Package retry 重试子进程调用中的瞬时网络故障。glab/gh 内部 HTTP
// 客户端的 dial/TLS 超时不对外开放配置，弱网环境下唯一的手段是
// 退避重试；每次重试在 stderr 留一行进度，长跑调用不再静默。
package retry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Attempts 是一次调用的最大尝试次数（含首次）：弱网实例要求
// 近乎"重试到成功"，100 次 × 最长 30s 间隔兜底约一小时。
const Attempts = 100

// maxBackoff 封顶单次退避时长。
const maxBackoff = 30 * time.Second

// Do 执行 fn：Transient 错误退避重试；成功、非瞬时错误或尝试
// 耗尽时返回最后一次结果。ctx 取消立即返回，不再重试。
func Do(ctx context.Context, label string, fn func() ([]byte, error)) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		out, err := fn()
		if err == nil || !Transient(err) || attempt >= Attempts {
			return out, err
		}
		delay := backoff(attempt)
		fmt.Fprintf(os.Stderr, "fastimpl: %s failed (attempt %d/%d), retrying in %s: %v\n",
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
	return false
}

// transientPatterns 是瞬时错误的文本特征（小写匹配）。
var transientPatterns = []string{
	"timeout", // i/o timeout、TLS handshake timeout、deadline exceeded
	"timed out",
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
