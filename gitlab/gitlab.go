// Package gitlab 通过 glab CLI 读写 GitLab：取 spec issue 的子任务、
// 阻塞关联，启动 worker 前领取（assignee=@me）并把 work item status
// 置为 In progress，完成时置 Done 后关闭 issue。
// 所有调用显式携带 --hostname/--repo，不依赖 glab 从 remote 推断。
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/retry"
)

// Runner 执行 glab 命令；测试用假实现替换它。
type Runner interface {
	// Run 在 dir 中执行 glab，把 stdin 送入标准输入并返回标准输出。
	Run(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error)
}

// ExecRunner 是调用真实 glab 的 Runner。
// glab 依赖当前目录里的 Git remote 推断项目，因此必须在仓库内执行。
type ExecRunner struct{}

// Run 在仓库目录中执行 glab，不经过 shell 拼接，标准错误并入错误信息。
// 瞬时网络故障（dial/TLS 超时、连接拒绝、上游 5xx）退避重试——glab
// 内部 HTTP 超时不对外配置，弱网实例只能靠重试扛抖动。
func (ExecRunner) Run(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error) {
	return retry.Do(ctx, "glab "+strings.Join(argv, " "), func() ([]byte, error) {
		return execGlab(ctx, dir, argv, stdin)
	})
}

// execGlab 执行单次 glab 调用（retry.Do 的重试体）。
func execGlab(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "glab", argv...)
	cmd.Dir = dir
	// 子进程不继承 FASTIMPL_LLM_*，避免凭据经 glab 泄漏。
	cmd.Env = config.ScrubbedEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("glab %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Probe 用 glab 探测 host 是否是可达的 GitLab 实例：执行
// `glab api --hostname <host> user`，连通且鉴权通过即认为上游是
// GitLab。它是 forge 推断的最后一环：当主机名启发式（如自建实例
// git.bfdx.com）判断不出时，由它确认 GitLab。
func Probe(ctx context.Context, runner Runner, dir, host string) bool {
	_, err := runner.Run(ctx, dir, []string{"api", "--hostname", host, "user"}, nil)
	return err == nil
}

// Issue 是一个 issue 的 REST 视图。
type Issue struct {
	IID         int    `json:"iid"`
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Type        string `json:"issue_type"`
	ProjectID   int    `json:"project_id"`
	Assignees   []struct {
		Username string `json:"username"`
	} `json:"assignees"`
}

// IsOpen 报告 issue 当前是否未关闭。
func (t Issue) IsOpen() bool { return t.State == "opened" }

// assignedUsernames 返回指派人的用户名。
func (t Issue) assignedUsernames() []string {
	out := make([]string, 0, len(t.Assignees))
	for _, assignee := range t.Assignees {
		out = append(out, assignee.Username)
	}
	return out
}

// AssignedToOther 报告 issue 是否已被当前用户之外的人领取。
func (t Issue) AssignedToOther(username string) bool {
	for _, name := range t.assignedUsernames() {
		if name != username {
			return true
		}
	}
	return false
}

// Child 是 GraphQL 层级查询返回的一个直接子任务。
type Child struct {
	IID   int
	Title string
	State string
	Type  string // workItemType.name，只取 "task"
}

// Link 是一个原生 issue 关联；只关心 is_blocked_by。
type Link struct {
	IID       int    `json:"iid"`
	ProjectID int    `json:"project_id"`
	State     string `json:"state"`
	LinkType  string `json:"link_type"`
}

// Client 是面向单个 GitLab 项目的 glab 适配器。
type Client struct {
	runner Runner
	// dir 是执行 glab 的仓库目录。
	dir string
	// host 是 origin 的主机名，作为每个调用的显式 GitLab 主机。
	host string
	// repoRef 是 "<host>/<origin path>"，作为写操作的显式 --repo。
	repoRef string
	// projectPath 是 origin 的完整项目路径。
	projectPath string
	// projectID 是 REST 使用的数字项目 ID。
	projectID int
	// username 是当前 glab 登录用户，用于领取判定。
	username string
	// repoRefs 缓存跨项目工作项的 project id → "host/path" 引用。
	repoRefs map[int]string
}

// NewClient 探测项目身份与当前用户。wantHost/wantPath 必须来自 origin
// remote（worktree.Repo.OriginCoordinate）；返回的项目与 origin 不一致
// 时拒绝继续，避免写入落到错误项目。
func NewClient(ctx context.Context, runner Runner, dir, wantHost, wantPath string) (*Client, error) {
	if wantHost == "" || wantPath == "" {
		return nil, errors.New("origin host and path are required; fastimpl will not guess the GitLab project")
	}
	// 用 origin 的路径直接请求项目，避免 projects/:id 依赖当前目录 remote 推断。
	projectJSON, err := runner.Run(ctx, dir, []string{"api", "--hostname", wantHost, "projects/" + escape(wantPath)}, nil)
	if err != nil {
		return nil, fmt.Errorf("read project identity: %w", err)
	}
	var project struct {
		ID                int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	}
	if err := json.Unmarshal(projectJSON, &project); err != nil {
		return nil, fmt.Errorf("decode project identity: %w", err)
	}
	if project.ID == 0 || project.PathWithNamespace == "" {
		return nil, errors.New("project identity is incomplete; run fastimpl inside the repository")
	}
	if project.PathWithNamespace != wantPath {
		return nil, fmt.Errorf("glab resolved project %q, but origin is %q; run fastimpl in the origin's checkout", project.PathWithNamespace, wantPath)
	}
	userJSON, err := runner.Run(ctx, dir, []string{"api", "--hostname", wantHost, "user"}, nil)
	if err != nil {
		return nil, fmt.Errorf("read current user: %w", err)
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(userJSON, &user); err != nil {
		return nil, fmt.Errorf("decode current user: %w", err)
	}
	if user.Username == "" {
		return nil, errors.New("current user is unknown")
	}
	return &Client{
		runner:      runner,
		dir:         dir,
		host:        wantHost,
		repoRef:     wantHost + "/" + wantPath,
		projectPath: project.PathWithNamespace,
		projectID:   project.ID,
		username:    user.Username,
		repoRefs:    map[int]string{},
	}, nil
}

// api 在以 origin 主机为显式参数的 glab 上执行一次只读 REST 调用。
func (c *Client) api(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"api", "--hostname", c.host}, args...)
	return c.runner.Run(ctx, c.dir, argv, nil)
}

// apiStdin 同 api，但把 payload 经标准输入送入（GraphQL 查询体）。
// 必须显式声明 Content-Type：--input 裸发 body 时 glab 不加任何
// Content-Type，Rails 侧不按 JSON 解析，GraphQL 拿到空 query 报
// "Unexpected end of document"。
func (c *Client) apiStdin(ctx context.Context, payload []byte, args ...string) ([]byte, error) {
	argv := append([]string{"api", "--hostname", c.host, "--input", "-",
		"-H", "Content-Type: application/json"}, args...)
	return c.runner.Run(ctx, c.dir, argv, payload)
}

// projectScope 返回 REST 端点里的项目范围：origin 用 URL 编码的路径，
// 其他项目用数字 ID。
func (c *Client) projectScope(projectID int) string {
	if projectID == c.projectID {
		return escape(c.projectPath)
	}
	return strconv.Itoa(projectID)
}

// escape 把项目路径编码成单个 REST 路径段。
func escape(path string) string {
	return url.PathEscape(path)
}

// IssueState 返回 issue 的开关、领取、占用与看板终态（claim 前重读与
// 周期刷新共用）。InFlight 表示看板 status 在 in_progress 类目
// 且未指派给当前用户——他人或别的 agent 正在处理，不应抢占；
// 指派给本人的 in progress 视为可续做，不算占用。
// 一次 GraphQL 查询同取 state/assignees/status widget；
// 不支持 status 的实例没有 STATUS widget，InFlight/DoneOnBoard
// 都按"无可判"处理。
func (c *Client) IssueState(ctx context.Context, projectID, iid int) (forge.IssueInfo, error) {
	var info forge.IssueInfo
	path, err := c.projectFullPath(ctx, projectID)
	if err != nil {
		return info, err
	}
	payload, err := json.Marshal(map[string]any{
		"query":     gateQuery,
		"variables": map[string]any{"path": path, "iid": strconv.Itoa(iid)},
	})
	if err != nil {
		return info, fmt.Errorf("encode gate query: %w", err)
	}
	raw, err := c.apiStdin(ctx, payload, "graphql")
	if err != nil {
		return info, fmt.Errorf("read state of %d!%d: %w", projectID, iid, err)
	}
	var reply gateReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return info, fmt.Errorf("decode state of %d!%d: %w", projectID, iid, err)
	}
	if len(reply.Errors) > 0 {
		return info, fmt.Errorf("read state of %d!%d: %s", projectID, iid, reply.Errors[0].Message)
	}
	if reply.Data.Project == nil || len(reply.Data.Project.WorkItems.Nodes) == 0 {
		return info, fmt.Errorf("work item %d!%d not found", projectID, iid)
	}
	item := reply.Data.Project.WorkItems.Nodes[0]
	info.Open = strings.EqualFold(item.State, "open") || strings.EqualFold(item.State, "opened")
	info.DoneOnBoard = true // 无 status widget 时无可判、无可修
	// 先收集指派人：widget 顺序不保证 assignees 在 status 之前，
	// 而 InFlight 判定需要它。
	assignedToMe := false
	for _, w := range item.Widgets {
		if w.Typename != "WorkItemWidgetAssignees" {
			continue
		}
		for _, a := range w.Assignees.Nodes {
			if a.Username == c.username {
				assignedToMe = true
			} else {
				info.AssignedToOther = true
			}
		}
	}
	for _, w := range item.Widgets {
		if w.Typename != "WorkItemWidgetStatus" {
			continue
		}
		info.DoneOnBoard = w.Status != nil && categoryDone(w.Status.Category)
		if w.Status != nil && statusInFlight(w.Status.Category) && !assignedToMe {
			info.InFlight = true
		}
	}
	// 已关闭的 issue 可能残留 In progress 状态（close 不改 status），
	// 只在仍开放时才视为占用。
	info.InFlight = info.InFlight && info.Open
	return info, nil
}

// statusInFlight 报告 status category 是否表示"进行中"
// （IN_PROGRESS/in_progress/inProgress 归一判定）。
func statusInFlight(category string) bool {
	return strings.EqualFold(strings.ReplaceAll(category, "_", ""), "inprogress")
}

// categoryDone 报告 status category 是否表示终态
// （DONE/CANCELED——已关闭 issue 留在此类目即为一致）。
func categoryDone(category string) bool {
	c := strings.ToLower(strings.ReplaceAll(category, "_", ""))
	return c == "done" || c == "canceled" || c == "cancelled"
}

// gateQuery 是 IssueState 的一次性查询：开关、指派人、status widget。
// WorkItem 没有顶层 assignees 字段，指派人挂在
// WorkItemWidgetAssignees widget 里。故意不查 namespace
// lifecycles——无 status 许可的命名空间查它会报错，而占用判定
// 只需要 widget 本身。
const gateQuery = `query($path: ID!, $iid: String!) { project(fullPath: $path) { workItems(iids: [$iid]) { nodes { state widgets { __typename ... on WorkItemWidgetStatus { status { name category } } ... on WorkItemWidgetAssignees { assignees { nodes { username } } } } } } } } }`

// gateReply 是 gateQuery 的应答结构。
type gateReply struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Data struct {
		Project *struct {
			WorkItems struct {
				Nodes []struct {
					State   string `json:"state"`
					Widgets []struct {
						Typename string `json:"__typename"`
						Status   *struct {
							Name     string `json:"name"`
							Category string `json:"category"`
						} `json:"status"`
						Assignees struct {
							Nodes []struct {
								Username string `json:"username"`
							} `json:"nodes"`
						} `json:"assignees"`
					} `json:"widgets"`
				} `json:"nodes"`
			} `json:"workItems"`
		} `json:"project"`
	} `json:"data"`
}

// Issue 读取指定项目中的一个 issue，并校验返回的项目与编号。
func (c *Client) Issue(ctx context.Context, projectID, iid int) (Issue, error) {
	raw, err := c.api(ctx, fmt.Sprintf("projects/%s/issues/%d", c.projectScope(projectID), iid))
	if err != nil {
		return Issue{}, fmt.Errorf("read issue %d!%d: %w", projectID, iid, err)
	}
	var issue Issue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Issue{}, fmt.Errorf("decode issue %d!%d: %w", projectID, iid, err)
	}
	if issue.IID != iid {
		return Issue{}, fmt.Errorf("issue %d!%d returned iid %d", projectID, iid, issue.IID)
	}
	if issue.ProjectID != 0 && issue.ProjectID != projectID {
		return Issue{}, fmt.Errorf("issue %d!%d returned project %d", projectID, iid, issue.ProjectID)
	}
	return issue, nil
}

// Spec 读取 spec issue，并要求它确实是 Issue 而不是 Task：父任务必须是
// 可以挂子任务的 spec。
func (c *Client) Spec(ctx context.Context, iid int) (Issue, error) {
	issue, err := c.Issue(ctx, c.projectID, iid)
	if err != nil {
		return Issue{}, err
	}
	switch strings.ToLower(issue.Type) {
	case "issue":
		return issue, nil
	case "task":
		return Issue{}, fmt.Errorf("issue %d is a Task; pass the spec Issue that parents the tasks", iid)
	}
	return Issue{}, fmt.Errorf("issue %d has unknown type %q", iid, issue.Type)
}

// Children 分页读取 spec issue 的全部直接子任务，拒绝错误、缺失 widget
// 和不前进的游标。
func (c *Client) Children(ctx context.Context, specIID int) ([]Child, error) {
	var out []Child
	after := ""
	for range maxHierarchyPages {
		nodes, next, err := c.childPage(ctx, specIID, after)
		if err != nil {
			return nil, err
		}
		out = append(out, nodes...)
		if next == "" {
			return out, nil
		}
		if next == after {
			return nil, fmt.Errorf("hierarchy cursor of issue %d did not advance", specIID)
		}
		after = next
	}
	return nil, fmt.Errorf("hierarchy of issue %d exceeds %d pages", specIID, maxHierarchyPages)
}

// maxHierarchyPages 限制层级分页次数，避免游标异常时无限循环。
const maxHierarchyPages = 100

// hierarchyReply 是层级查询的应答结构。
type hierarchyReply struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Data struct {
		Project *struct {
			WorkItems struct {
				Nodes []struct {
					IID     string `json:"iid"`
					Widgets []struct {
						Typename string `json:"__typename"`
						Children struct {
							Nodes []struct {
								IID          string `json:"iid"`
								Title        string `json:"title"`
								State        string `json:"state"`
								WorkItemType struct {
									Name string `json:"name"`
								} `json:"workItemType"`
							} `json:"nodes"`
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"children"`
					} `json:"widgets"`
				} `json:"nodes"`
			} `json:"workItems"`
		} `json:"project"`
	} `json:"data"`
}

// childPage 请求一页子任务，返回节点与下一页游标。
func (c *Client) childPage(ctx context.Context, specIID int, after string) ([]Child, string, error) {
	variables := map[string]any{"path": c.projectPath, "iid": strconv.Itoa(specIID)}
	if after != "" {
		variables["after"] = after
	}
	payload, err := json.Marshal(map[string]any{"query": hierarchyQuery, "variables": variables})
	if err != nil {
		return nil, "", fmt.Errorf("encode hierarchy query: %w", err)
	}
	raw, err := c.apiStdin(ctx, payload, "graphql")
	if err != nil {
		return nil, "", fmt.Errorf("read hierarchy of issue %d: %w", specIID, err)
	}
	var reply hierarchyReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, "", fmt.Errorf("decode hierarchy of issue %d: %w", specIID, err)
	}
	if len(reply.Errors) > 0 {
		return nil, "", fmt.Errorf("hierarchy of issue %d: %s", specIID, reply.Errors[0].Message)
	}
	if reply.Data.Project == nil || len(reply.Data.Project.WorkItems.Nodes) == 0 {
		return nil, "", fmt.Errorf("hierarchy of issue %d: work item not found", specIID)
	}
	item := reply.Data.Project.WorkItems.Nodes[0]
	if item.IID != strconv.Itoa(specIID) {
		return nil, "", fmt.Errorf("hierarchy of issue %d returned iid %q", specIID, item.IID)
	}
	for _, widget := range item.Widgets {
		if widget.Typename != "WorkItemWidgetHierarchy" {
			continue
		}
		nodes := make([]Child, 0, len(widget.Children.Nodes))
		for _, child := range widget.Children.Nodes {
			iid, err := strconv.Atoi(child.IID)
			if err != nil {
				return nil, "", fmt.Errorf("hierarchy of issue %d: child iid %q is not a number", specIID, child.IID)
			}
			nodes = append(nodes, Child{IID: iid, Title: child.Title, State: child.State, Type: child.WorkItemType.Name})
		}
		next := ""
		if widget.Children.PageInfo.HasNextPage {
			next = widget.Children.PageInfo.EndCursor
			if next == "" {
				return nil, "", fmt.Errorf("hierarchy of issue %d: page reports more children without a cursor", specIID)
			}
		}
		return nodes, next, nil
	}
	return nil, "", fmt.Errorf("hierarchy of issue %d: work item has no hierarchy widget", specIID)
}

// hierarchyQuery 是读取父 issue 直接子任务的 GraphQL 查询。
const hierarchyQuery = `query($path: ID!, $iid: String!, $after: String) { project(fullPath: $path) { workItems(iids: [$iid]) { nodes { iid widgets { __typename ... on WorkItemWidgetHierarchy { children(first: 100, after: $after) { nodes { iid title state workItemType { name } } pageInfo { hasNextPage endCursor } } } } } } } }`

// Links 读取一个 issue 的原生关联（可跨项目）；is_blocked_by 等
// 具体语义由调用方（graph.Source 适配器）过滤。
func (c *Client) Links(ctx context.Context, projectID, iid int) ([]Link, error) {
	raw, err := c.api(ctx, "--paginate", fmt.Sprintf("projects/%s/issues/%d/links", c.projectScope(projectID), iid))
	if err != nil {
		return nil, fmt.Errorf("read links of %d!%d: %w", projectID, iid, err)
	}
	// --paginate 依次输出多页数组；逐页解码直到输入耗尽。
	var out []Link
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for decoder.More() {
		var page []Link
		if err := decoder.Decode(&page); err != nil {
			return nil, fmt.Errorf("decode links of %d!%d: %w", projectID, iid, err)
		}
		out = append(out, page...)
	}
	return out, nil
}

// repoRef 把项目 database id 解析成 "host/path" 引用；缓存未命中时经
// `glab api projects/<id>` 查询并登记缓存。
func (c *Client) repoRefFor(ctx context.Context, projectID int) (string, error) {
	if projectID == c.projectID {
		return c.repoRef, nil
	}
	if ref, ok := c.repoRefs[projectID]; ok {
		return ref, nil
	}
	raw, err := c.api(ctx, "projects/"+strconv.Itoa(projectID))
	if err != nil {
		return "", fmt.Errorf("resolve project id %d: %w", projectID, err)
	}
	var project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	}
	if err := json.Unmarshal(raw, &project); err != nil || project.PathWithNamespace == "" {
		return "", fmt.Errorf("resolve project id %d: no path_with_namespace", projectID)
	}
	ref := c.host + "/" + project.PathWithNamespace
	c.repoRefs[projectID] = ref
	return ref, nil
}

// issueCmd 在指定项目上执行 glab issue 子命令（update/close/note）。
func (c *Client) issueCmd(ctx context.Context, projectID, iid int, argv ...string) error {
	ref, err := c.repoRefFor(ctx, projectID)
	if err != nil {
		return err
	}
	args := append([]string{"issue"}, append(argv, "--repo", ref)...)
	_, err = c.runner.Run(ctx, c.dir, args, nil)
	return err
}

// Claim 把 issue 指派给当前用户（--assignee @me）。
// 调用方必须在启动 worker 前重新读取状态与指派；被他人领取的任务不得启动。
func (c *Client) Claim(ctx context.Context, projectID, iid int) error {
	return c.issueCmd(ctx, projectID, iid, "update", strconv.Itoa(iid), "--assignee", "@me")
}

// 默认看板状态名：GitLab 系统生命周期与 GitHub 原生枚举同名；
// 群组自定义 lifecycle 改了名字时需要相应调整。
const (
	statusInProgress = "In progress"
	statusDone       = "Done"
)

// MarkInProgress 把 issue 的 work item status 置为 In progress。
// 实例不支持 status（CE/低版本无 widget）或状态名不在 lifecycle 中
// 时报错，由调用方降级为 warn——状态是辅助标记，不阻塞任务。
func (c *Client) MarkInProgress(ctx context.Context, projectID, iid int) error {
	return c.setStatus(ctx, projectID, iid, statusInProgress)
}

// MarkDone 把 issue 的 work item status 置为 Done；Done 类目会自动
// 关闭 issue，调用方仍会照常执行 Close 兜底。
func (c *Client) MarkDone(ctx context.Context, projectID, iid int) error {
	return c.setStatus(ctx, projectID, iid, statusDone)
}

// setStatus 把 issue/task 的 work item status 置为 name：
// 一次 GraphQL 查询取回 work item GID、类型、当前 status 与根命名
// 空间全部 lifecycle 状态表，解析出目标 status 的 GID 后经
// workItemUpdate 写入；已是目标状态时跳过写。
func (c *Client) setStatus(ctx context.Context, projectID, iid int, name string) error {
	path, err := c.projectFullPath(ctx, projectID)
	if err != nil {
		return err
	}
	payload, err := statusQueryPayload(path, iid)
	if err != nil {
		return err
	}
	raw, err := c.apiStdin(ctx, payload, "graphql")
	if err != nil {
		return fmt.Errorf("read status of %d!%d: %w", projectID, iid, err)
	}
	var reply statusReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("decode status of %d!%d: %w", projectID, iid, err)
	}
	if len(reply.Errors) > 0 {
		return fmt.Errorf("read status of %d!%d: %s", projectID, iid, reply.Errors[0].Message)
	}
	if reply.Data.Project == nil || len(reply.Data.Project.WorkItems.Nodes) == 0 {
		return fmt.Errorf("work item %d!%d not found", projectID, iid)
	}
	item := reply.Data.Project.WorkItems.Nodes[0]
	var current string
	supported := false
	for _, w := range item.Widgets {
		if w.Typename == "WorkItemWidgetStatus" {
			supported = true
			if w.Status != nil {
				current = w.Status.Name
			}
		}
	}
	if !supported {
		return fmt.Errorf("%d!%d: work item status unsupported on this instance", projectID, iid)
	}
	if strings.EqualFold(current, name) {
		return nil
	}
	gid, err := statusGID(reply.Data.Namespace, item.WorkItemType.Name, name)
	if err != nil {
		return fmt.Errorf("resolve status %q for %d!%d: %w", name, projectID, iid, err)
	}
	return c.updateWorkItemStatus(ctx, item.ID, gid)
}

// projectFullPath 返回项目的完整路径：origin 直接用 projectPath，
// 跨项目经 repoRefFor 解析（返回的 "host/path" 去掉主机名）。
func (c *Client) projectFullPath(ctx context.Context, projectID int) (string, error) {
	if projectID == c.projectID {
		return c.projectPath, nil
	}
	ref, err := c.repoRefFor(ctx, projectID)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(ref, c.host+"/"), nil
}

// statusQueryPayload 编码 work item status 查询：同一请求取回目标
// work item 的身份/类型/当前 status 与根命名空间的 lifecycle 状态表。
// lifecycle 挂在 root namespace 上，取项目路径第一段。
func statusQueryPayload(path string, iid int) ([]byte, error) {
	ns, _, _ := strings.Cut(path, "/")
	return json.Marshal(map[string]any{
		"query": statusQuery,
		"variables": map[string]any{
			"path": path,
			"ns":   ns,
			"iid":  strconv.Itoa(iid),
		},
	})
}

// statusQuery 同时查询 work item 与根命名空间 lifecycle。
const statusQuery = `query($path: ID!, $ns: ID!, $iid: String!) { project(fullPath: $path) { workItems(iids: [$iid]) { nodes { id workItemType { name } widgets { __typename ... on WorkItemWidgetStatus { status { name } } } } } } namespace(fullPath: $ns) { lifecycles { nodes { workItemTypes { name } statuses { id name } } } } }`

// statusReply 是 statusQuery 的应答结构。
type statusReply struct {
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Data struct {
		Project *struct {
			WorkItems struct {
				Nodes []struct {
					ID           string `json:"id"`
					WorkItemType struct {
						Name string `json:"name"`
					} `json:"workItemType"`
					Widgets []struct {
						Typename string `json:"__typename"`
						Status   *struct {
							Name string `json:"name"`
						} `json:"status"`
					} `json:"widgets"`
				} `json:"nodes"`
			} `json:"workItems"`
		} `json:"project"`
		Namespace *struct {
			Lifecycles struct {
				Nodes []lifecycleNode `json:"nodes"`
			} `json:"lifecycles"`
		} `json:"namespace"`
	} `json:"data"`
}

// lifecycleNode 是一个命名空间 lifecycle：适用的 work item 类型
// 与该 lifecycle 下的全部状态。
type lifecycleNode struct {
	WorkItemTypes []struct {
		Name string `json:"name"`
	} `json:"workItemTypes"`
	Statuses []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"statuses"`
}

// statusGID 按 work item 类型找到对应 lifecycle，再按状态名解析
// 目标 status 的 GID；无 lifecycle 或状态名不存在时报错。
func statusGID(ns *struct {
	Lifecycles struct {
		Nodes []lifecycleNode `json:"nodes"`
	} `json:"lifecycles"`
}, typeName, want string) (string, error) {
	if ns != nil {
		for _, lc := range ns.Lifecycles.Nodes {
			typed := false
			for _, t := range lc.WorkItemTypes {
				if strings.EqualFold(t.Name, typeName) {
					typed = true
					break
				}
			}
			if !typed {
				continue
			}
			for _, st := range lc.Statuses {
				if strings.EqualFold(st.Name, want) {
					return st.ID, nil
				}
			}
			return "", fmt.Errorf("lifecycle for %s has no status %q", typeName, want)
		}
	}
	return "", fmt.Errorf("no lifecycle found for work item type %q", typeName)
}

// updateWorkItemStatus 经 workItemUpdate mutation 写入目标 status GID。
func (c *Client) updateWorkItemStatus(ctx context.Context, workItemGID, statusGID string) error {
	payload, err := json.Marshal(map[string]any{
		"query": statusMutation,
		"variables": map[string]any{
			"id":     workItemGID,
			"status": statusGID,
		},
	})
	if err != nil {
		return fmt.Errorf("encode status update: %w", err)
	}
	raw, err := c.apiStdin(ctx, payload, "graphql")
	if err != nil {
		return fmt.Errorf("update work item status: %w", err)
	}
	var reply struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			WorkItemUpdate *struct {
				Errors []string `json:"errors"`
			} `json:"workItemUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("decode status update: %w", err)
	}
	if len(reply.Errors) > 0 {
		return fmt.Errorf("update work item status: %s", reply.Errors[0].Message)
	}
	if reply.Data.WorkItemUpdate == nil {
		return errors.New("update work item status: empty response")
	}
	if len(reply.Data.WorkItemUpdate.Errors) > 0 {
		return fmt.Errorf("update work item status: %s", reply.Data.WorkItemUpdate.Errors[0])
	}
	return nil
}

// statusMutation 把 work item 的 status widget 置为目标 status GID。
const statusMutation = `mutation($id: WorkItemID!, $status: WorkItemStatusID!) { workItemUpdate(input: {id: $id, statusWidget: {status: $status}}) { errors } }`

// Close 关闭一个 issue。
func (c *Client) Close(ctx context.Context, projectID, iid int) error {
	return c.issueCmd(ctx, projectID, iid, "close", strconv.Itoa(iid))
}

// Note 在 issue 上追加一条评论。
func (c *Client) Note(ctx context.Context, projectID, iid int, message string) error {
	return c.issueCmd(ctx, projectID, iid, "note", strconv.Itoa(iid), "--message", message)
}

// EnsureMR 确保 sourceBranch → targetBranch 的 MR 存在：
// 已有 opened MR 返回其 web_url；否则经 REST 创建并返回 web_url。
// 集成分支在建好后立即调用一次，此后每次集成 push 都会更新该 MR。
func (c *Client) EnsureMR(ctx context.Context, projectID int, sourceBranch, targetBranch, title string) (string, error) {
	scope := c.projectScope(projectID)
	raw, err := c.api(ctx, fmt.Sprintf("projects/%s/merge_requests?state=opened&source_branch=%s",
		scope, url.QueryEscape(sourceBranch)))
	if err != nil {
		return "", fmt.Errorf("list merge requests for %s: %w", sourceBranch, err)
	}
	var mrs []struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &mrs); err != nil {
		return "", fmt.Errorf("decode merge requests for %s: %w", sourceBranch, err)
	}
	if len(mrs) > 0 && mrs[0].WebURL != "" {
		return mrs[0].WebURL, nil
	}
	raw, err = c.api(ctx, "-X", "POST", "projects/"+scope+"/merge_requests",
		"-f", "source_branch="+sourceBranch,
		"-f", "target_branch="+targetBranch,
		"-f", "title="+title)
	if err != nil {
		return "", fmt.Errorf("create merge request %s -> %s: %w", sourceBranch, targetBranch, err)
	}
	var mr struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &mr); err != nil || mr.WebURL == "" {
		return "", fmt.Errorf("decode created merge request for %s", sourceBranch)
	}
	return mr.WebURL, nil
}

// Username 返回当前 glab 登录用户。
func (c *Client) Username() string { return c.username }

// ProjectID 返回 origin 项目的数字 ID。
func (c *Client) ProjectID() int { return c.projectID }
