package app

import (
	"context"
	"strings"

	"github.com/yangjuncode/fastimpl/github"
	"github.com/yangjuncode/fastimpl/gitlab"
	"github.com/yangjuncode/fastimpl/graph"
)

// glabSource 把 gitlab.Client 包装成 graph.Source。
type glabSource struct {
	c *gitlab.Client
}

func (s glabSource) Spec(ctx context.Context, iid int) (graph.IssueInfo, error) {
	issue, err := s.c.Spec(ctx, iid)
	if err != nil {
		return graph.IssueInfo{}, err
	}
	return toIssueInfo(issue), nil
}

func (s glabSource) Children(ctx context.Context, iid int) ([]graph.ChildRef, error) {
	children, err := s.c.Children(ctx, iid)
	if err != nil {
		return nil, err
	}
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
	return out, nil
}

func (s glabSource) Issue(ctx context.Context, k graph.Key) (graph.IssueInfo, error) {
	issue, err := s.c.Issue(ctx, k.ProjectID, k.IID)
	if err != nil {
		return graph.IssueInfo{}, err
	}
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

// ghSource 把 github.Client 包装成 graph.Source。
type ghSource struct {
	c *github.Client
}

func (s ghSource) Spec(ctx context.Context, num int) (graph.IssueInfo, error) {
	issue, err := s.c.Spec(ctx, num)
	if err != nil {
		return graph.IssueInfo{}, err
	}
	return toGHIssueInfo(github.RepoIssue{Issue: issue, RepoID: s.c.RepoID()}), nil
}

func (s ghSource) Children(ctx context.Context, num int) ([]graph.ChildRef, error) {
	children, err := s.c.Children(ctx, num)
	if err != nil {
		return nil, err
	}
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
	return out, nil
}

func (s ghSource) Issue(ctx context.Context, k graph.Key) (graph.IssueInfo, error) {
	issue, err := s.c.Issue(ctx, k.ProjectID, k.IID)
	if err != nil {
		return graph.IssueInfo{}, err
	}
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
