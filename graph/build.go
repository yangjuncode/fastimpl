package graph

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
//  1. src.Spec 校验父 issue；src.Children 与 spec 自身 blocked-by
//     并行拉取；type=task 的直接子任务作为 KindChild 种子。
//  2. 并发 BFS 传递闭包：fetchWorkers 个 worker 各并发读取节点的
//     Issue 与 BlockedBy → 前置 = BlockedBy ∪ 正文顶部
//     "Blocked by: #n"（同项目 ref）→ 记入 Deps；未访问过的前置
//     重新入队（已关闭的也展开，保证图完整可画）。
//  3. 节点上限 maxNodes；循环依赖经拓扑检测报错。
//  4. 完成后每个开放节点都是潜在工作项：外部 blocker 与子任务一样
//     走 claim → worktree → agent → 集成 → close 流程。
func Build(ctx context.Context, src Source, specIID int) (*Graph, error) {
	spec, err := src.Spec(ctx, specIID)
	if err != nil {
		return nil, err
	}
	g := New(spec.Key)
	g.specTitle = spec.Title

	// 出错即取消，让在途的 glab/gh 子进程尽快退出。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 子任务列表与 spec 自身的 blocked-by 并行拉取。
	var children []ChildRef
	var specLinks []Key
	var childErr, specErr error
	var specWG sync.WaitGroup
	specWG.Add(2)
	go func() {
		defer specWG.Done()
		children, childErr = src.Children(ctx, specIID)
	}()
	go func() {
		defer specWG.Done()
		specLinks, specErr = src.BlockedBy(ctx, spec.Key)
	}()
	specWG.Wait()
	if childErr != nil {
		return nil, childErr
	}
	if specErr != nil {
		return nil, specErr
	}
	g.specDeps = mergeDeps(specLinks, spec.Key.ProjectID, spec.Description)

	type pending struct {
		key  Key
		kind Kind
	}
	seen := map[Key]bool{spec.Key: true}
	var queue []pending
	push := func(k Key, kind Kind) {
		if !seen[k] {
			seen[k] = true
			queue = append(queue, pending{k, kind})
		}
	}
	for _, child := range children {
		if !strings.EqualFold(child.Type, "task") {
			continue // 只把 task 类型的子项作为工作项
		}
		push(child.Key, KindChild)
	}
	for _, dep := range g.specDeps {
		push(dep, KindExternal)
	}
	if len(queue) == 0 {
		return nil, fmt.Errorf("issue %d has no child tasks", specIID)
	}

	jobs := make(chan pending)
	results := make(chan fetchResult)
	var pool sync.WaitGroup
	for range fetchWorkers {
		pool.Add(1)
		go func() {
			defer pool.Done()
			for j := range jobs {
				results <- fetchNode(ctx, src, j)
			}
		}()
	}
	// 单线程消费：queue/seen/g.nodes 无需加锁。outstanding = 已派发
	// 未回报的任务数；队空且无在途即结束。
	outstanding := 0
	var firstErr error
	for len(queue) > 0 || outstanding > 0 {
		var jobCh chan pending
		var next pending
		if len(queue) > 0 {
			jobCh, next = jobs, queue[0]
		}
		select {
		case jobCh <- next:
			queue = queue[1:]
			outstanding++
		case r := <-results:
			outstanding--
			if r.err != nil {
				if firstErr == nil {
					firstErr = r.err
					cancel()
				}
				queue = nil
				continue
			}
			if firstErr != nil {
				continue
			}
			if len(g.nodes) >= maxNodes {
				firstErr = fmt.Errorf("dependency graph exceeds %d nodes", maxNodes)
				cancel()
				queue = nil
				continue
			}
			g.Add(Node{
				Key:    r.key,
				Title:  r.info.Title,
				Type:   r.info.Type,
				Kind:   r.kind,
				Closed: !r.info.Open,
				Deps:   r.deps,
			})
			for _, dep := range r.deps {
				push(dep, KindExternal)
			}
		}
	}
	close(jobs)
	pool.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if cycle := g.findCycle(); len(cycle) > 0 {
		return nil, fmt.Errorf("dependency cycle detected: %s", joinKeys(cycle))
	}
	return g, nil
}

// fetchWorkers 是建图 BFS 的并发度；每任务内部再并发 Issue 与
// BlockedBy 两次调用。
const fetchWorkers = 8

// fetchResult 是一个节点的拉取结果：详情、前置与错误。
type fetchResult struct {
	key  Key
	kind Kind
	info IssueInfo
	deps []Key
	err  error
}

// fetchNode 读取一个节点：Issue 详情与 blocked-by 关联并发发起，
// 前置合并原生关联与正文顶部降级行。
func fetchNode(ctx context.Context, src Source, j struct {
	key  Key
	kind Kind
}) fetchResult {
	res := fetchResult{key: j.key, kind: j.kind}
	var links []Key
	var infoErr, linksErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		res.info, infoErr = src.Issue(ctx, j.key)
	}()
	go func() {
		defer wg.Done()
		links, linksErr = src.BlockedBy(ctx, j.key)
	}()
	wg.Wait()
	switch {
	case infoErr != nil:
		res.err = fmt.Errorf("read issue %s: %w", j.key, infoErr)
	case linksErr != nil:
		res.err = fmt.Errorf("read links of %s: %w", j.key, linksErr)
	default:
		res.deps = mergeDeps(links, res.info.Key.ProjectID, res.info.Description)
	}
	return res
}

// mergeDeps 合并原生 blocked-by 关联与正文顶部的 "Blocked by: #n"
// 降级表示（正文 ref 按同项目处理），去重后返回。
func mergeDeps(links []Key, projectID int, description string) []Key {
	seen := make(map[Key]bool, len(links))
	var deps []Key
	for _, dep := range links {
		if !seen[dep] {
			seen[dep] = true
			deps = append(deps, dep)
		}
	}
	for _, iid := range bodyDeps(description) {
		dep := Key{ProjectID: projectID, IID: iid}
		if !seen[dep] {
			seen[dep] = true
			deps = append(deps, dep)
		}
	}
	return deps
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
