package app

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/yangjuncode/fastimpl/scheduler"
)

// ActiveRunConflictError 表示显式指定的 spec 仍由另一个 live supervisor
// 管理。它是结构化错误：CLI 只把它当拒绝原因打印，GUI 可以据此切换到
// 只读观察页，而不是把 active worker 当成一次启动失败。
type ActiveRunConflictError struct {
	SpecIID int
	Tasks   []scheduler.ActiveTaskMeta
}

func (e *ActiveRunConflictError) Error() string {
	if len(e.Tasks) == 0 {
		return fmt.Sprintf("spec #%d is actively supervised by another fastimpl process", e.SpecIID)
	}
	first := e.Tasks[0]
	names := make([]string, 0, len(e.Tasks))
	for _, task := range e.Tasks {
		if task.TerminalName != "" {
			names = append(names, task.TerminalName)
		}
	}
	hint := ""
	if len(names) > 0 {
		hint = "; observe with hc attach " + strings.Join(names, " or hc attach ")
	}
	return fmt.Sprintf("spec #%d is actively supervised by fastimpl instance %s pid %d (heartbeat %s)%s",
		e.SpecIID, first.Supervisor.InstanceID, first.Supervisor.PID,
		first.Supervisor.HeartbeatAt.Format(time.RFC3339), hint)
}

// checkActiveSpecBeforeWrites 是显式 spec 启动的 preflight。它必须发生在
// 集成分支同步、push、EnsureMR 或任何 worker 操作之前：live supervisor 只读，
// blocked 保守失败，dead/adoptable 才交给 scheduler 接管。
func checkActiveSpecBeforeWrites(repoDir string, specIID int, now time.Time) error {
	metas, invalid, err := scheduler.ScanActiveTasksDetailed(repoDir)
	if err != nil {
		return fmt.Errorf("scan active fastimpl runs: %w", err)
	}
	if len(invalid) > 0 {
		return fmt.Errorf("invalid or unsupported active fastimpl metadata exists (%s); inspect the original supervisor/worker and clean it manually before taking over", strings.Join(invalid, ", "))
	}
	tasks := make([]scheduler.ActiveTaskMeta, 0)
	for _, meta := range metas {
		if meta.SpecIID == specIID {
			tasks = append(tasks, meta)
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	choice := scheduler.ChooseActiveSpec(tasks, now)
	switch choice.State {
	case scheduler.ActiveRecoveryObserve:
		return &ActiveRunConflictError{SpecIID: specIID, Tasks: choice.Tasks}
	case scheduler.ActiveRecoveryBlocked:
		return fmt.Errorf("active fastimpl metadata for spec #%d is blocked or unsafe; check the original supervisor/heartbeat before taking over", specIID)
	}
	return nil
}

// recoverSpecFromActive 在 CLI 未输入 spec 时扫描本仓库 active metadata。
// 返回值 handled 表示“只能只读观察”的情况已经向用户报告完毕，调用方应
// 直接退出而不进入调度；spec 非零表示选择了可自动接管的原 spec。
func recoverSpecFromActive(repoDir string, stderr io.Writer, log *slog.Logger) (spec int, handled bool, err error) {
	metas, invalid, err := scheduler.ScanActiveTasksDetailed(repoDir)
	if err != nil {
		return 0, false, fmt.Errorf("scan active fastimpl runs: %w", err)
	}
	if len(invalid) > 0 {
		return 0, false, fmt.Errorf("invalid or unsupported active fastimpl metadata exists (%s); inspect the original supervisor/worker and clean it manually before taking over", strings.Join(invalid, ", "))
	}
	if len(metas) == 0 {
		return 0, false, nil
	}
	choice := scheduler.ChooseActiveSpec(metas, time.Now())
	switch choice.State {
	case scheduler.ActiveRecoveryAdoptable:
		log.Info("active fastimpl run is recoverable; resuming its spec", "spec", choice.SpecIID, "tasks", len(choice.Tasks))
		return choice.SpecIID, false, nil
	case scheduler.ActiveRecoveryObserve:
		fmt.Fprintf(stderr, "spec #%d is actively supervised by another fastimpl process; this CLI instance will only report it, not take control\n", choice.SpecIID)
		for _, task := range choice.Tasks {
			fmt.Fprintf(stderr, "  issue #%d: hc terminal %s (%s), supervisor pid %d, heartbeat %s",
				task.IssueIID, task.TerminalName, task.TerminalAddr, task.Supervisor.PID, task.Supervisor.HeartbeatAt.Format(time.RFC3339))
			if task.TerminalName != "" {
				fmt.Fprintf(stderr, "; inspect with: hc attach %s", task.TerminalName)
			}
			fmt.Fprintln(stderr)
		}
		return 0, true, nil
	case scheduler.ActiveRecoveryBlocked:
		return 0, false, fmt.Errorf("active fastimpl metadata for spec #%d is blocked or unsafe; check the original supervisor/heartbeat before taking over", choice.SpecIID)
	}
	return 0, false, nil
}
