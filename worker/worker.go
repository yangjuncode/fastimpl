// Package worker 通过 herdr-core（github.com/yangjuncode/hcgo）管理
// Devin CLI 会话：每个子任务一个 hc 终端，在其 worktree 中运行 devin，
// 发送渲染后的提示词，轮询 OSC 7501 状态直到一轮结束。
// GUI 版本把每个会话映射为一个 nsgo terminal tab，运行中可直接对话。
package worker

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	hc "github.com/yangjuncode/hcgo"
)

const (
	// InitialStatusBudget 是等待 worker 首次上报 OSC 7501 状态的时限。
	InitialStatusBudget = 5 * time.Minute
	// TurnBudget 是等待一轮 turn 结束的时限。
	TurnBudget = 90 * time.Minute
	// IntegrationTurnBudget 是集成专属 agent 解一轮 base 冲突的时限，
	// 比 TurnBudget 短：merge 冲突解决不应像完整实现那样久等。
	IntegrationTurnBudget = 45 * time.Minute
	// PollInterval 是轮询终端记录的间隔。
	PollInterval = 3 * time.Second
	// CaptureLines 是读取屏幕的行数：0 表示只要当前屏幕，不读 scrollback。
	CaptureLines = 0
)

// Params 是启动一个 worker 终端所需的全部参数。
type Params struct {
	// SpecIID 是 spec issue IID，用于终端命名与归属。
	SpecIID int
	// IssueIID 是子任务 issue IID。
	IssueIID int
	// Name 是终端名，与任务分支同名（issue-<iid> 或带描述后缀），
	// 便于 `hc attach <name>` 直接进入；空时退化为 fastimpl-<spec>-<iid>。
	Name string
	// Worktree 是终端的工作目录（该任务的 worktree）。
	Worktree string
	// Argv 是 agent 命令行（如 devin --model …），由 config.AgentArgv 给出。
	Argv []string
}

// Record 是一次终端观测所需的字段，与 hc 的记录解耦以便测试。
type Record struct {
	// Turn 是当前轮次编号。
	Turn uint64
	// Status 是程序通过 OSC 7501 自报的状态词（working/idle/done/blocked/error）。
	Status string
	// Reported 表示程序是否曾上报状态。
	Reported bool
	// Live 表示进程仍在运行。
	Live bool
}

// Settled 报告该记录是否表示一轮已经结束。
func (r Record) Settled() bool {
	return r.Status == "done" || r.Status == "idle"
}

// Runner 是与 herdr-core 的连接，管理全部 worker 终端。
type Runner struct {
	client *hc.Client
	// mu 串行化写入，保证同一 turn 内不并发发送。
	mu sync.Mutex
}

// Dial 连接本机或远程 herdr-core：hcSocket 非空直连 Unix socket，
// 否则用 hcBinary（空时按 PATH 找 hc）执行 `hc connect`。
func Dial(ctx context.Context, hcBinary, hcSocket string) (*Runner, error) {
	opts := hc.Options{Socket: hcSocket}
	if hcSocket == "" {
		opts.Binary = hcBinary
	}
	client, err := hc.Dial(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("connect herdr-core: %w", err)
	}
	return &Runner{client: client}, nil
}

// taskLabels 返回属于某任务的终端标签选择器：task 用完整终端名
// （<repo>-issue-<iid>），多项目并行时不会误匹配。
func taskLabels(specIID int, name string) map[string]string {
	return map[string]string{
		"fastimpl": strconv.Itoa(specIID),
		"task":     name,
	}
}

// StartWorker 在该任务 worktree 中启动 agent 交互终端并返回终端地址。
// LLM 密钥变量显式清空，避免 core 继承到的值泄漏进 worker 环境。
func (r *Runner) StartWorker(ctx context.Context, p Params) (addr string, err error) {
	if len(p.Argv) == 0 {
		return "", fmt.Errorf("start worker in %s: empty agent argv", p.Worktree)
	}
	env := map[string]string{
		config.LLMAPIKeyEnv:  "",
		config.LLMBaseURLEnv: "",
		config.LLMModelEnv:   "",
	}
	name := p.Name
	if name == "" {
		name = fmt.Sprintf("fastimpl-%d-%d", p.SpecIID, p.IssueIID)
	}
	term, err := r.client.Run(hc.RunParams{
		Argv:   p.Argv,
		Cwd:    p.Worktree,
		Env:    env,
		Name:   name,
		Labels: taskLabels(p.SpecIID, name),
	})
	if err != nil {
		return "", fmt.Errorf("start worker in %s: %w", p.Worktree, err)
	}
	return term.Address, nil
}

// Send 把文本输入终端；guard 非空时作为状态前置条件（如 "idle"），
// 由 core 判定，避免把提示词打进正在工作的 agent 或未处理的询问。
// 返回发送时的轮次与输入是否真的启动了这一轮。
func (r *Runner) Send(ctx context.Context, addr, text, guard string) (turn uint64, started bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result, err := r.client.Send(addr, text, hc.SendOptions{If: guard})
	if err != nil {
		return 0, false, err
	}
	return result.Turn, result.Started != nil && *result.Started, nil
}

// Snapshot 读取终端的 7501 状态记录。
func (r *Runner) Snapshot(ctx context.Context, addr string) (Record, error) {
	term, err := r.client.Terminal(addr)
	if err != nil {
		return Record{}, err
	}
	return Record{
		Turn:     term.Turn,
		Status:   term.StatusWord(),
		Reported: term.Reported,
		Live:     term.Live(),
	}, nil
}

// ReadScreen 读取终端屏幕文本（lines=0 只要当前屏幕，不读 scrollback），
// 供提取 FASTIMPL-READY 报告帧；complete=false 表示屏幕不完整。
func (r *Runner) ReadScreen(ctx context.Context, addr string, lines int) (text string, complete bool, err error) {
	result, err := r.client.Read(addr, lines)
	if err != nil {
		return "", false, err
	}
	return result.Text, result.Complete, nil
}

// Attach 打开终端的观看流（可写、可改尺寸），供 GUI tab 或
// `hc attach` 等价物使用。
func (r *Runner) Attach(ctx context.Context, addr string, cols, rows int) (io.ReadWriteCloser, error) {
	return r.client.Attach(addr, cols, rows, hc.AttachOptions{})
}

// Stop 结束身份完全匹配的终端：地址、标签都必须命中才 Kill，
// 防止误伤其他会话。taskName 是启动时的完整终端名。
func (r *Runner) Stop(ctx context.Context, addr string, specIID int, taskName string) error {
	if addr == "" {
		return nil
	}
	terminals, err := r.client.Terminals(false, taskLabels(specIID, taskName))
	if err != nil {
		return fmt.Errorf("list terminals for %s: %w", taskName, err)
	}
	for _, term := range terminals {
		if term.Address == addr {
			if !term.Live() {
				return nil
			}
			if _, err := r.client.Kill(addr); err != nil {
				return fmt.Errorf("kill terminal %s: %w", addr, err)
			}
			return nil
		}
	}
	return fmt.Errorf("terminal %s does not match %s labels; refusing to stop", addr, taskName)
}

// Close 关闭与 core 的连接；core 上的程序继续运行。
func (r *Runner) Close() error {
	r.client.Close()
	return nil
}

// AwaitBoot 等到 worker 的终端可用：live 且状态落在 idle/done。
// 权限询问与工作区信任弹窗会让状态停在 blocked/working，此时继续等待，
// 由人在终端里处理；只有终端结束或超时才算失败。
func (r *Runner) AwaitBoot(ctx context.Context, addr string) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, InitialStatusBudget)
	defer cancel()
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		record, err := r.Snapshot(ctx, addr)
		if err != nil {
			return Record{}, err
		}
		if !record.Live {
			return record, fmt.Errorf("worker terminal %s ended before it accepted input", addr)
		}
		if record.Settled() {
			return record, nil
		}
		select {
		case <-ctx.Done():
			return record, fmt.Errorf("worker terminal %s never settled within %s (last %s)", addr, InitialStatusBudget, record.Status)
		case <-ticker.C:
		}
	}
}

// Prompt 在终端空闲时发送提示词；返回本轮的基线轮次（发送前后的
// 较大值），供 WaitTurn 判定轮次真的前进。
func (r *Runner) Prompt(ctx context.Context, addr, text string) (baseline uint64, err error) {
	before, err := r.Snapshot(ctx, addr)
	if err != nil {
		return 0, err
	}
	if !before.Settled() {
		return 0, fmt.Errorf("worker terminal %s is %s, not ready for a prompt", addr, before.Status)
	}
	turn, started, err := r.Send(ctx, addr, text, before.Status)
	if err != nil {
		return 0, fmt.Errorf("send prompt to %s: %w", addr, err)
	}
	if !started {
		return 0, fmt.Errorf("worker terminal %s did not accept the prompt", addr)
	}
	return max(before.Turn, turn), nil
}

// WaitTurn 等待一轮结束，返回该轮结束时的记录。
// 只有轮次超过基线且状态落在成功终态时才算完成；error/failed 返回错误。
func (r *Runner) WaitTurn(ctx context.Context, addr string, baseline uint64, budget time.Duration) (Record, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		record, err := r.Snapshot(ctx, addr)
		if err != nil {
			return Record{}, err
		}
		if !record.Live {
			return record, fmt.Errorf("worker terminal %s ended during the turn", addr)
		}
		if record.Turn > baseline {
			switch record.Status {
			case "error", "failed":
				return record, fmt.Errorf("worker terminal %s ended the turn with %s", addr, record.Status)
			case "done", "idle":
				return record, nil
			}
		}
		select {
		case <-ctx.Done():
			return record, fmt.Errorf("worker terminal %s did not settle within %s", addr, budget)
		case <-ticker.C:
		}
	}
}
