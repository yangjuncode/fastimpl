package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/scheduler"
	"github.com/yangjuncode/fastimpl/worktree"
)

// AttachTerminal 打开一个任务终端的观看流：addr 是 scheduler 公开的
// 终端地址，cols/rows 是初始网格。返回的流可读可写，离屏时关闭即可——
// 关闭观看不会结束 worker。
type AttachTerminal func(ctx context.Context, addr string, cols, rows int) (io.ReadWriteCloser, error)

// Observer 是一次运行对外的事件。CLI 传零值（全部为 nil），GUI 用它
// 跟随构图与调度。回调都在运行自己的 goroutine 上同步调用，不会并发，
// 也绝不在界面线程上做 IO；实现方应当快速返回，需要 UI 更新就转投
// 到自己的调度队列。
type Observer struct {
	// Progress 接收建图的阶段与计数，目标数随闭包展开动态增长。
	Progress func(graph.BuildProgress)
	// GraphReady 在依赖图建好、任何写入或调度启动之前调用。
	GraphReady func(*graph.Graph)
	// SchedulerReady 在调度器组装完成、Run 启动之前调用；attach 用
	// 与调度同源的 hc 配置另开观看连接。
	SchedulerReady func(*scheduler.Scheduler, AttachTerminal)
}

// RepositoryError 表示目标仓库无法解析或不可用（找不到仓库、有歧义、
// 不是仓库顶层、origin 缺失或不可解析）。GUI 据此弹仓库设置让用户
// 指定目录后重试；普通配置、forge 与 API 错误不会包成这个类型。
type RepositoryError struct {
	err error
	// Dir 是被拒绝的目录（能确定时），供设置界面预填。
	Dir string
}

// Error 返回包裹的错误文本，与未包裹时完全一致。
func (e *RepositoryError) Error() string { return e.err.Error() }

// Unwrap 返回原始错误，供 errors.Is/As 继续判断。
func (e *RepositoryError) Unwrap() error { return e.err }

// repoError 把一个仓库解析错误包成 *RepositoryError；nil 原样返回。
func repoError(dir string, err error) error {
	if err == nil {
		return nil
	}
	return &RepositoryError{err: err, Dir: dir}
}

// IsRepositoryError 报告 err 是否是（或包裹了）仓库解析错误。
func IsRepositoryError(err error) bool {
	var target *RepositoryError
	return errors.As(err, &target)
}

// observing 报告观察者是否想跟随这次运行。只读的 -print 用零值
// Observer，因此不需要任何额外行为。
func (o Observer) observing() bool {
	return o.Progress != nil || o.GraphReady != nil || o.SchedulerReady != nil
}

// verifyRepoRoot 确认 dir 本身就是 git 仓库顶层。git 命令在子目录里
// 也能跑，`git remote get-url origin` 会一路查到父仓库的 origin，
// 把用户的目录认成另一个项目的 checkout——显式路径必须自己就是顶层，
// 否则 GUI 应当让用户重新选目录。
func verifyRepoRoot(ctx context.Context, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return repoError(dir, fmt.Errorf("resolve repo-dir %s: %w", dir, err))
	}
	abs = filepath.Clean(abs)
	top, err := worktree.NewRepo(abs, "").TopLevel(ctx, abs)
	if err != nil {
		return repoError(abs, fmt.Errorf("repo-dir %s is not a git repository: %w", abs, err))
	}
	if top != abs {
		return repoError(abs, fmt.Errorf("repo-dir %s is not the top level of its git repository (that is %s); pick the repository's own directory", abs, top))
	}
	return nil
}
