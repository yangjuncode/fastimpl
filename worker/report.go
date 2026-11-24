package worker

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReadyMarker 是完成报告的唯一标记：agent 在最终回复末尾输出
// 单独一行 `FASTIMPL-READY <token>`；提交以 worktree HEAD 为准，
// 不采信 agent 自报的 SHA。
const ReadyMarker = "FASTIMPL-READY"

// ErrNoFrame 表示屏幕文本中没有本运行令牌的报告行。
var ErrNoFrame = errors.New("no report frame on screen")

// Report 是从终端屏幕提取的一次完成报告。
type Report struct {
	// Token 是本运行的令牌，必须与会话令牌一致。
	Token string
	// Lines 是 READY 行之后最多六行的结论说明（可为空）。
	Lines []string
	// Text 是从 READY 行起的报告文本（供日志/人工查看）。
	Text string
	// Turn 是报告所在轮次（由提取方填）。
	Turn uint64
}

// ParseReport 从屏幕文本中取出属于本次运行的最后一个
// `FASTIMPL-READY <token>` 行及其后最多六行结论。
// 令牌不匹配的帧视为过期证据；找不到返回 ErrNoFrame。
func ParseReport(screen, token string) (Report, error) {
	trimmed := strings.TrimRight(screen, " \t\n")
	marker := ReadyMarker + " " + token
	start := strings.LastIndex(trimmed, marker)
	if start < 0 {
		return Report{}, fmt.Errorf("%w: no %s line for this run", ErrNoFrame, ReadyMarker)
	}
	rest := trimmed[start+len(marker):]
	// 令牌后必须紧跟空白或结束，防止匹配到同前缀的更长了令牌。
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' && rest[0] != '\n' {
		return Report{}, fmt.Errorf("%w: malformed %s line %q", ErrNoFrame, ReadyMarker, strings.TrimSpace(trimmed[start:]))
	}
	// 报告正文可能跟在令牌同一行（agent 未严格换行），取该剩余
	// 行加后续行，共至多六行；终端硬换行产生的续行同样收集。
	parts := strings.Split(rest, "\n")
	rep := Report{Token: token}
	end := 0
	for i := 0; i < len(parts) && len(rep.Lines) < 6; i++ {
		line := strings.TrimSpace(parts[i])
		if line == "" {
			if len(rep.Lines) > 0 {
				break
			}
			continue
		}
		rep.Lines = append(rep.Lines, line)
		end = i + 1
	}
	if end == 0 {
		rep.Text = marker
	} else {
		rep.Text = marker + strings.Join(parts[:end], "\n")
	}
	return rep, nil
}

// ContinuePrompt 是重试时续发给同一终端的提示词：要求 agent
// 继续完成工作并在末尾输出报告标记。
func ContinuePrompt(token string) string {
	return "继续完成这项工作：实现、验证与两轮审核都通过后，在回复末尾单独一行输出 " + ReadyMarker + " " + token + "，随后最多六行说明结论。未达到条件则继续处理，不要输出该标记。"
}

// NewToken 生成一次运行/任务的报告令牌，用于隔离过期屏幕证据。
func NewToken() string {
	var b [8]byte
	// crypto/rand 失败时退回时间戳，令牌只要同运行唯一即可。
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("t%x", b[:])
}
