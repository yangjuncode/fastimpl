package graph

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// maxNodes 限制传递闭包拉入的节点总数，防止异常关联把图撑爆。
const maxNodes = 512

// blockedByLine 匹配 issue 正文顶部的 "Blocked by: #1, #2" 降级表示。
// 只解析顶部行，不扫描整篇 Markdown。
var blockedByLine = regexp.MustCompile(`(?m)^Blocked by:\s*(#[0-9]+(?:\s*,\s*#[0-9]+)*)\s*$`)

// issueRef 提取一行中的全部 #编号。
var issueRef = regexp.MustCompile(`#([0-9]+)`)

// ChildRef 是 spec 的直接子任务；GitHub sub-issue 可跨仓库，
// 因此携带完整 Key 而不只是编号。
type ChildRef struct {
	Key   Key
	Title string
	State string
	Type  string // workItemType.name
}

// IssueInfo 是建图时读取的节点详情。
type IssueInfo struct {
	Key         Key
	Title       string
	Type        string
	Open        bool   // issue 未关闭
	Description string // 正文，供 "Blocked by: #n" 降级解析
}

// Source 提供建图所需的只读查询；由 app 层把 gitlab.Client（或
// github.Client）包装成本接口注入。
type Source interface {
	// Spec 读取父 issue 并校验它是可挂子任务的类型。
	Spec(ctx context.Context, iid int) (IssueInfo, error)
	// Children 读取 spec 的全部直接子任务。
	Children(ctx context.Context, iid int) ([]ChildRef, error)
	// BlockedBy 返回本项 blocked-by 的目标（可跨项目）。
	BlockedBy(ctx context.Context, k Key) ([]Key, error)
	// Issue 读取节点详情（标题/类型/开关/正文）。
	Issue(ctx context.Context, k Key) (IssueInfo, error)
}

// Build 建立 spec 的完整依赖图：
//  1. src.Spec 校验父 issue；src.Children 取直接子任务（type=task），
//     作为 KindChild 种子。
//  2. BFS 传递闭包：取节点 → src.Issue 读详情 → 前置 = src.BlockedBy
//     ∪ 正文顶部 "Blocked by: #n"（同项目 ref）→ 记入 Deps；
//     未访问过的前置入队（已关闭的也继续展开，保证图完整可画）；
//     spec 自身的 blocked-by 同样拉入闭包并记入 specDeps（展示用）。
//  3. 节点上限 maxNodes；循环依赖经拓扑检测报错。
//  4. 完成后每个开放节点都是潜在工作项：外部 blocker 与子任务一样
//     走 claim → worktree → agent → 集成 → close 流程。
func Build(ctx context.Context, src Source, specIID int) (*Graph, error) {
	spec, err := src.Spec(ctx, specIID)
	if err != nil {
		return nil, err
	}
	g := New(spec.Key)

	specDeps, err := unionDeps(ctx, src, spec)
	if err != nil {
		return nil, err
	}
	g.specTitle = spec.Title
	g.specDeps = specDeps

	children, err := src.Children(ctx, specIID)
	if err != nil {
		return nil, err
	}
	type pending struct {
		key  Key
		kind Kind
	}
	seen := map[Key]bool{spec.Key: true}
	var queue []pending
	for _, child := range children {
		if !strings.EqualFold(child.Type, "task") {
			continue // 只把 task 类型的子项作为工作项
		}
		if !seen[child.Key] {
			seen[child.Key] = true
			queue = append(queue, pending{child.Key, KindChild})
		}
	}
	for _, dep := range specDeps {
		if !seen[dep] {
			seen[dep] = true
			queue = append(queue, pending{dep, KindExternal})
		}
	}
	if len(queue) == 0 {
		return nil, fmt.Errorf("issue %d has no child tasks", specIID)
	}

	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if len(g.nodes) >= maxNodes {
			return nil, fmt.Errorf("dependency graph exceeds %d nodes", maxNodes)
		}
		info, err := src.Issue(ctx, item.key)
		if err != nil {
			return nil, err
		}
		deps, err := unionDeps(ctx, src, info)
		if err != nil {
			return nil, err
		}
		g.Add(Node{
			Key:    item.key,
			Title:  info.Title,
			Type:   info.Type,
			Kind:   item.kind,
			Closed: !info.Open,
			Deps:   deps,
		})
		for _, dep := range deps {
			if !seen[dep] {
				seen[dep] = true
				queue = append(queue, pending{dep, KindExternal})
			}
		}
	}
	if cycle := g.findCycle(); len(cycle) > 0 {
		return nil, fmt.Errorf("dependency cycle detected: %s", joinKeys(cycle))
	}
	return g, nil
}

// unionDeps 合并原生 blocked-by 关联与正文顶部的 "Blocked by: #n"
// 降级表示（正文 ref 按同项目处理），去重后返回。
func unionDeps(ctx context.Context, src Source, info IssueInfo) ([]Key, error) {
	linked, err := src.BlockedBy(ctx, info.Key)
	if err != nil {
		return nil, err
	}
	seen := make(map[Key]bool, len(linked))
	var deps []Key
	for _, dep := range linked {
		if !seen[dep] {
			seen[dep] = true
			deps = append(deps, dep)
		}
	}
	for _, iid := range bodyDeps(info.Description) {
		dep := Key{ProjectID: info.Key.ProjectID, IID: iid}
		if !seen[dep] {
			seen[dep] = true
			deps = append(deps, dep)
		}
	}
	return deps, nil
}

// bodyDeps 提取正文顶部 "Blocked by: #n" 行中的全部编号。
func bodyDeps(description string) []int {
	match := blockedByLine.FindStringSubmatch(description)
	if len(match) < 2 {
		return nil
	}
	var out []int
	for _, ref := range issueRef.FindAllStringSubmatch(match[1], -1) {
		if iid, err := strconv.Atoi(ref[1]); err == nil {
			out = append(out, iid)
		}
	}
	return out
}

// findCycle 返回一个依赖环上的 Key；无环返回 nil。DFS 三色标记。
func (g *Graph) findCycle() []Key {
	g.mu.Lock()
	defer g.mu.Unlock()
	const (
		white = 0 // 未访问
		gray  = 1 // 栈上
		black = 2 // 完成
	)
	color := make(map[Key]int, len(g.nodes))
	var stack []Key
	var visit func(k Key) []Key
	visit = func(k Key) []Key {
		color[k] = gray
		stack = append(stack, k)
		for _, dep := range g.nodes[k].Deps {
			if _, ok := g.nodes[dep]; !ok {
				continue
			}
			switch color[dep] {
			case gray:
				// 环起点：stack 中 dep 之后的部分构成环。
				for i, key := range stack {
					if key == dep {
						return append([]Key{}, stack[i:]...)
					}
				}
			case white:
				if cycle := visit(dep); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[k] = black
		return nil
	}
	for key := range g.nodes {
		if color[key] == white {
			if cycle := visit(key); cycle != nil {
				return cycle
			}
		}
	}
	return nil
}

// joinKeys 把一组 Key 拼成可读序列。
func joinKeys(keys []Key) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k.String()
	}
	return strings.Join(parts, " -> ")
}
