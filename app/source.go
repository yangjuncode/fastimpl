package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/yangjuncode/fastimpl/github"
	"github.com/yangjuncode/fastimpl/gitlab"
	"github.com/yangjuncode/fastimpl/graph"
)

// glabSource 把 gitlab.Client 包装成 graph.Source。log 用于逐节点
// 打印建图进度——弱网下每次读取都可能耗时数秒，静默等待会让人
// 以为卡死。
type glabSource struct {
	c   *gitlab.Client
	log *slog.Logger
}

func (s glabSource) Spec(ctx context.Context, iid int) (graph.IssueInfo, error) {
	issue, err := s.c.Spec(ctx, iid)
	if err != nil {
		return graph.IssueInfo{}, err
	}
	s.log.Info("spec issue", "key", graph.Key{ProjectID: s.c.ProjectID(), IID: iid}, "title", issue.Title)
	return toIssueInfo(issue), nil
}

func (s glabSource) Children(ctx context.Context, iid int) ([]graph.ChildRef, error) {
	children, err := s.c.Children(ctx, iid)
	if err != nil {
		return nil, err
	}
	s.log.Info("spec children", "spec", iid, "count", len(children))
	out := make([]graph.ChildRef, len(children))
	for i, child := range children {
		out[i] = graph.ChildRef{
			Key:   graph.Key{ProjectID: s.c.ProjectID(), IID: child.IID},
			Title: child.Title, State: child.State, Type: child.Type,
		}
	}
	return out, nil
}

func (s glabSource) BlockedBy(ctx context.Context, k graph.Key) ([]graph.Key, error) {
	links, err := s.c.Links(ctx, k.ProjectID, k.IID)
	if err != nil {
		return nil, err
	}
	var out []graph.Key
	for _, link := range links {
		if !strings.EqualFold(link.LinkType, "is_blocked_by") {
			continue
		}
		out = append(out, graph.Key{ProjectID: link.ProjectID, IID: link.IID})
	}
	if len(out) > 0 {
		s.log.Info("issue blocked-by", "key", k, "deps", out)
	}
	return out, nil
}

func (s glabSource) Issue(ctx context.Context, k graph.Key) (graph.IssueInfo, error) {
	issue, err := s.c.Issue(ctx, k.ProjectID, k.IID)
	if err != nil {
		return graph.IssueInfo{}, err
	}
	s.log.Info("graph node", "key", k, "state", issue.State, "title", issue.Title)
	return toIssueInfo(issue), nil
}

// toIssueInfo 把 gitlab.Issue 映射为 graph.IssueInfo。
func toIssueInfo(issue gitlab.Issue) graph.IssueInfo {
	return graph.IssueInfo{
		Key:         graph.Key{ProjectID: issue.ProjectID, IID: issue.IID},
		Title:       issue.Title,
		Type:        issue.Type,
		Open:        issue.IsOpen(),
		Description: issue.Description,
	}
}

// ghSource 把 github.Client 包装成 graph.Source（log 作用同上）。
type ghSource struct {
	c   *github.Client
	log *slog.Logger
}

func (s ghSource) Spec(ctx context.Context, num int) (graph.IssueInfo, error) {
	issue, err := s.c.Spec(ctx, num)
	if err != nil {
		return graph.IssueInfo{}, err
	}
	s.log.Info("spec issue", "key", graph.Key{ProjectID: s.c.RepoID(), IID: num}, "title", issue.Title)
	return toGHIssueInfo(github.RepoIssue{Issue: issue, RepoID: s.c.RepoID()}), nil
}

func (s ghSource) Children(ctx context.Context, num int) ([]graph.ChildRef, error) {
	children, err := s.c.Children(ctx, num)
	if err != nil {
		return nil, err
	}
	s.log.Info("spec children", "spec", num, "count", len(children))
	out := make([]graph.ChildRef, len(children))
	for i, child := range children {
		// GitHub sub-issues 没有 GitLab 式 type 过滤，全部按工作项处理；
		// 跨仓库子项携带各自 RepoID。
		out[i] = graph.ChildRef{
			Key:   graph.Key{ProjectID: child.RepoID, IID: child.Number},
			Title: child.Title, State: child.State, Type: "task",
		}
	}
	return out, nil
}

func (s ghSource) BlockedBy(ctx context.Context, k graph.Key) ([]graph.Key, error) {
	issues, err := s.c.BlockedBy(ctx, k.ProjectID, k.IID)
	if err != nil {
		return nil, err
	}
	out := make([]graph.Key, 0, len(issues))
	for _, issue := range issues {
		out = append(out, graph.Key{ProjectID: issue.RepoID, IID: issue.Number})
	}
	if len(out) > 0 {
		s.log.Info("issue blocked-by", "key", k, "deps", out)
	}
	return out, nil
}

func (s ghSource) Issue(ctx context.Context, k graph.Key) (graph.IssueInfo, error) {
	issue, err := s.c.Issue(ctx, k.ProjectID, k.IID)
	if err != nil {
		return graph.IssueInfo{}, err
	}
	s.log.Info("graph node", "key", k, "state", issue.State, "title", issue.Title)
	return toGHIssueInfo(github.RepoIssue{Issue: issue, RepoID: k.ProjectID}), nil
}

// toGHIssueInfo 把 github.RepoIssue 映射为 graph.IssueInfo。
func toGHIssueInfo(issue github.RepoIssue) graph.IssueInfo {
	return graph.IssueInfo{
		Key:         graph.Key{ProjectID: issue.RepoID, IID: issue.Number},
		Title:       issue.Title,
		Type:        issue.TypeName(),
		Open:        issue.IsOpen(),
		Description: issue.Body,
	}
}
