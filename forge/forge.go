// Package forge 定义 fastimpl 与代码托管平台（GitLab/GitHub）交互的
// 抽象接口与共享数据类型：调度器消费这些操作，gitlab/github 客户端实现。
package forge

import (
	"context"
	"time"
)

// IssueInfo 是一次 issue 现场状态读取的结果（claim 前重读与周期刷新共用）。
type IssueInfo struct {
	// Open 表示 issue 在 forge 上仍开放。
	Open bool
	// AssignedToMe 表示当前登录用户是指派人之一。
	AssignedToMe bool
	// AssignedToOther 表示已指派给当前用户以外的人。
	AssignedToOther bool
	// InFlight 表示看板 status 为进行中且未指派给当前用户——他人或
	// 别的 agent 正在处理，不应抢占；仅在 Open 时才可能为 true。
	InFlight bool
	// DoneOnBoard 表示看板 status 已在终态类目（Done/Won't do 等）；
	// 实例不支持原生 status（无 widget/preview 未启用）时恒为 true——
	// 没有看板可判，也无可修。
	DoneOnBoard bool
}

// IssueComment 是 issue 上的一条评论（note）。
type IssueComment struct {
	// ID 是服务端评论 ID，用于稳定排序与日志定位。
	ID int64
	// AuthorID 是作者的服务端用户 ID。
	AuthorID int64
	// CreatedAt 是服务端创建时间，仲裁按它排序。
	CreatedAt time.Time
	// Body 是评论正文。
	Body string
	// System 表示 forge 自动生成的状态/字段变更记录（GitLab system note）。
	// 这类记录对实现任务通常是噪音，生成给 agent 的快照时会过滤。
	System bool
}

// Forge 是调度器需要的 issue/MR 操作；gitlab.Client 与 github.Client
// 都满足本接口（GitLab 的 projectID 即 GitHub 的 repo database id）。
type Forge interface {
	// IssueState 读取 issue 的开关、领取、占用与看板终态。
	IssueState(ctx context.Context, projectID, iid int) (IssueInfo, error)
	// IssueComments 按服务端顺序读出 issue 的全部评论。
	IssueComments(ctx context.Context, projectID, iid int) ([]IssueComment, error)
	// Claim 把 issue 指派给当前用户。
	Claim(ctx context.Context, projectID, iid int) error
	// MarkInProgress 把 issue 的看板状态置为进行中（尽力而为：
	// 实例不支持该功能时调用方降级为 warn，不阻塞任务）。
	MarkInProgress(ctx context.Context, projectID, iid int) error
	// MarkDone 把 issue 的看板状态置为完成；同为尽力而为。
	MarkDone(ctx context.Context, projectID, iid int) error
	// Close 关闭一个 issue。
	Close(ctx context.Context, projectID, iid int) error
	// Note 在 issue 上追加一条评论。
	Note(ctx context.Context, projectID, iid int, message string) error
	// EnsureMR 确保集成分支到目标分支的 MR/PR 存在，返回其 URL。
	EnsureMR(ctx context.Context, projectID int, sourceBranch, targetBranch, title, description string) (string, error)
}
