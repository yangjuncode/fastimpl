// Package github 通过 gh CLI 读写 GitHub：sub_issues 取子任务、
// dependencies/blocked_by 取阻塞关联、启动 worker 前领取
// （--add-assignee @me）并把原生 status 置为 In progress，
// 完成时置 Done 后关闭 issue。
//
// 与 GitLab 的差异（API 调研结论）：
//   - 子任务是原生 sub-issues（可跨同 owner 仓库，≤100/parent，8 层深）；
//   - blocked-by 是 dependencies API：GET/POST/DELETE
//     /repos/{o}/{r}/issues/{n}/dependencies/blocked_by，
//     仅 github.com 支持，GHES 没有——404 时降级为只解析正文
//     "Blocked by: #n"；
//   - issue 有三种 ID：number（路径/UI）、REST database id（写操作
//     body 用）、GraphQL node_id；graph.Key{ProjectID,IID} 对应
//     {repo 数据库 id, issue number}；
//   - issue status 是 2026-11-23 起的公开预览功能：PATCH issue 写
//     status 字段，未加入 preview 的账号/GHES 会被静默忽略，
//     因此写入后回读响应验证；
//   - 所有调用显式带 --hostname 与 -R/--repo，不依赖 gh 推断。
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/retry"
)

// Runner 执行 gh 命令；测试用假实现替换它。
type Runner interface {
	// Run 在 dir 中执行 gh，把 stdin 送入标准输入并返回标准输出。
	Run(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error)
}

// ExecRunner 是调用真实 gh 的 Runner。
type ExecRunner struct {
	// Progress 接收重试进度；为空时写进程 stderr。GUI 传入自己的
	// 日志流，否则弱网重试在日志页/log_file 里不可见。
	Progress io.Writer
}

// Run 在仓库目录中执行 gh，不经过 shell 拼接，标准错误并入错误信息。
// 瞬时网络故障退避重试（见 retry.DoWriter）。
func (r ExecRunner) Run(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error) {
	return retry.DoWriter(ctx, "gh "+strings.Join(argv, " "), r.Progress, func() ([]byte, error) {
		return execGh(ctx, dir, argv, stdin)
	})
}

// execGh 执行单次 gh 调用（retry.Do 的重试体）。
func execGh(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", argv...)
	cmd.Dir = dir
	// 子进程不继承 FASTIMPL_LLM_*，避免凭据经 gh 泄漏。
	cmd.Env = config.ScrubbedEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Probe 用 gh 探测 host 是否是可达的 GitHub 实例：执行
// `gh api --hostname <host> user`，连通且鉴权通过即认为上游是 GitHub。
func Probe(ctx context.Context, runner Runner, dir, host string) bool {
	_, err := runner.Run(ctx, dir, []string{"api", "--hostname", host, "user"}, nil)
	return err == nil
}

// Issue 是一个 issue 的 REST 视图。
type Issue struct {
	Number int    `json:"number"` // 仓库内编号（对应 graph.Key.IID）
	ID     int    `json:"id"`     // REST database id（写操作 body 用）
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"` // open/closed
	// Status 是原生看板状态（2026-11 公开预览）；preview 未启用时为 null。
	Status *struct {
		Name     string `json:"name"`
		Category string `json:"category"`
	} `json:"status"`
	Type *struct {
		Name string `json:"name"`
	} `json:"type"`
	RepositoryURL string `json:"repository_url"` // 跨仓库关联时解析 repo id
	Assignees     []struct {
		Login string `json:"login"`
	} `json:"assignees"`
}

// IsOpen 报告 issue 当前是否未关闭。
func (i Issue) IsOpen() bool { return i.State == "open" }

// comment 是 GitHub issue comment 的 REST 视图。
type comment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

// AssignedToMe 报告当前用户是否是指派人之一。
func (i Issue) AssignedToMe(login string) bool {
	for _, a := range i.Assignees {
		if strings.EqualFold(a.Login, login) {
			return true
		}
	}
	return false
}

// AssignedToOther 报告 issue 是否已被当前用户之外的人领取。
func (i Issue) AssignedToOther(login string) bool {
	for _, a := range i.Assignees {
		if !strings.EqualFold(a.Login, login) {
			return true
		}
	}
	return false
}

// inFlightStatuses 是原生 status 中"已被占用"的枚举名：
// In progress 正在处理、In review 已送审——接手会与现有人冲突。
// GitHub status 是固定系统枚举，按名判定即可。
var inFlightStatuses = map[string]bool{"in progress": true, "in review": true}

// doneStatuses 是原生 status 的终态枚举名：已关闭 issue 停在这些
// 状态即为看板一致；停在 todo 列（Backlog/Todo/进行中）则不一致。
var doneStatuses = map[string]bool{"done": true, "not planned": true, "duplicate": true}

// DoneOnBoard 报告看板 status 是否已在终态；preview 未启用
// （status 为 null）时视为无可判，返回 true。
func (i Issue) DoneOnBoard() bool {
	return i.Status == nil || doneStatuses[strings.ToLower(i.Status.Name)]
}

// InFlight 报告 issue 是否被他人/其他 agent 占用：看板 status 是
// 进行中枚举且未指派给当前用户；指派给本人的视为可续做，不算占用。
func (i Issue) InFlight(login string) bool {
	if i.Status == nil || !inFlightStatuses[strings.ToLower(i.Status.Name)] {
		return false
	}
	return !i.AssignedToMe(login)
}

// TypeName 返回 issue type 名（GitHub issue types，可缺省）。
func (i Issue) TypeName() string {
	if i.Type == nil {
		return "issue"
	}
	return i.Type.Name
}

// RepoIssue 是 issue 连同其所在仓库的 database id；
// sub-issues 与 blocked-by 都可能跨同 owner 的仓库。
type RepoIssue struct {
	Issue
	RepoID int // 仓库 REST database id（对应 graph.Key.ProjectID）
}

// Client 是面向单个 GitHub 仓库的 gh 适配器。
type Client struct {
	runner Runner
	// dir 是执行 gh 的仓库目录。
	dir string
	// host 是 origin 的主机名（github.com 或 GHES），作为每个调用的显式主机。
	host string
	// repoRef 是 "owner/name"，作为写操作的显式 -R/--repo。
	repoRef string
	// repoID 是仓库的 REST database id（对应 graph.Key.ProjectID）。
	repoID int
	// login 是当前 gh 登录用户，用于领取判定。
	login string
	// repoNames 缓存 repo database id → "owner/name"；
	// repoIDs 缓存反向映射。跨仓库关联在返回时登记，
	// Issue/写操作经 repoNames 解析目标仓库。
	repoNames map[int]string
	repoIDs   map[string]int
}

// NewClient 探测项目身份与当前用户。wantPath 形如 "owner/repo"，必须来自
// origin remote；返回的仓库与 origin 不一致时拒绝继续。
func NewClient(ctx context.Context, runner Runner, dir, wantHost, wantPath string) (*Client, error) {
	if wantHost == "" || wantPath == "" {
		return nil, errors.New("origin host and path are required; fastimpl will not guess the GitHub repo")
	}
	repoJSON, err := runner.Run(ctx, dir, []string{"api", "--hostname", wantHost, "repos/" + wantPath}, nil)
	if err != nil {
		return nil, fmt.Errorf("read repo identity: %w", err)
	}
	var repo struct {
		ID       int    `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(repoJSON, &repo); err != nil {
		return nil, fmt.Errorf("decode repo identity: %w", err)
	}
	if repo.ID == 0 || repo.FullName == "" {
		return nil, errors.New("repo identity is incomplete; run fastimpl inside the repository")
	}
	if !strings.EqualFold(repo.FullName, wantPath) {
		return nil, fmt.Errorf("gh resolved repo %q, but origin is %q; run fastimpl in the origin's checkout", repo.FullName, wantPath)
	}
	userJSON, err := runner.Run(ctx, dir, []string{"api", "--hostname", wantHost, "user"}, nil)
	if err != nil {
		return nil, fmt.Errorf("read current user: %w", err)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(userJSON, &user); err != nil {
		return nil, fmt.Errorf("decode current user: %w", err)
	}
	if user.Login == "" {
		return nil, errors.New("current user is unknown")
	}
	return &Client{
		runner:    runner,
		dir:       dir,
		host:      wantHost,
		repoRef:   repo.FullName,
		repoID:    repo.ID,
		login:     user.Login,
		repoNames: map[int]string{repo.ID: repo.FullName},
		repoIDs:   map[string]int{repo.FullName: repo.ID},
	}, nil
}

// api 在以 origin 主机为显式参数的 gh 上执行一次只读 REST 调用。
func (c *Client) api(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"api", "--hostname", c.host}, args...)
	return c.runner.Run(ctx, c.dir, argv, nil)
}

// apiJSON 以显式 JSON body（经标准输入）执行一次 gh api 调用：
// -f 不会按数组语法转 JSON，需要复合结构时只能走 --input -。
func (c *Client) apiJSON(ctx context.Context, body any, args ...string) error {
	argv := append([]string{"api", "--hostname", c.host}, args...)
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode request body: %w", err)
		}
		argv = append(argv, "--input", "-")
	}
	_, err := c.runner.Run(ctx, c.dir, argv, payload)
	return err
}

// isNotFound 判定 gh api 的错误是否是 404（GHES 没有 dependencies API
// 时据此降级；也用于 issue/parent 的可选检查）。
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "404") || strings.Contains(msg, "Not Found")
}

// repoPath 把 repo database id 解析成 "owner/name"；缓存未命中时经
// `gh api repositories/<id>` 查询并登记双向缓存。
func (c *Client) repoPath(ctx context.Context, repoID int) (string, error) {
	if repoID == c.repoID {
		return c.repoRef, nil
	}
	if path, ok := c.repoNames[repoID]; ok {
		return path, nil
	}
	raw, err := c.api(ctx, fmt.Sprintf("repositories/%d", repoID))
	if err != nil {
		return "", fmt.Errorf("resolve repo id %d: %w", repoID, err)
	}
	var repo struct {
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(raw, &repo); err != nil || repo.FullName == "" {
		return "", fmt.Errorf("resolve repo id %d: no full_name", repoID)
	}
	c.repoNames[repoID] = repo.FullName
	c.repoIDs[repo.FullName] = repoID
	return repo.FullName, nil
}

// repoIdentity 把 "owner/name" 解析成 repo database id 并登记缓存；
// 已知路径直接返回。
func (c *Client) repoIdentity(ctx context.Context, path string) (int, error) {
	if path == c.repoRef {
		return c.repoID, nil
	}
	if id, ok := c.repoIDs[path]; ok {
		return id, nil
	}
	raw, err := c.api(ctx, "repos/"+path)
	if err != nil {
		return 0, fmt.Errorf("resolve repo %s: %w", path, err)
	}
	var repo struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &repo); err != nil || repo.ID == 0 {
		return 0, fmt.Errorf("resolve repo %s: no id", path)
	}
	c.repoNames[repo.ID] = path
	c.repoIDs[path] = repo.ID
	return repo.ID, nil
}

// pathFromIssueURL 从 issue.repository_url（形如
// https://api.github.com/repos/owner/name）提取 "owner/name"。
func pathFromIssueURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(u.Path, "/repos/"), "/")
}

// bindRepo 确定 issue 所在仓库的 database id：repository_url 指向的
// 仓库不是 origin 时解析/登记其 id。
func (c *Client) bindRepo(ctx context.Context, issue Issue) (RepoIssue, error) {
	path := pathFromIssueURL(issue.RepositoryURL)
	if path == "" || strings.EqualFold(path, c.repoRef) {
		return RepoIssue{Issue: issue, RepoID: c.repoID}, nil
	}
	id, err := c.repoIdentity(ctx, path)
	if err != nil {
		return RepoIssue{}, err
	}
	return RepoIssue{Issue: issue, RepoID: id}, nil
}

// decodePages 把 `gh api --paginate` 输出的连续 JSON 数组逐页解码。
func decodePages[T any](raw []byte, what string) ([]T, error) {
	var out []T
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for decoder.More() {
		var page []T
		if err := decoder.Decode(&page); err != nil {
			return nil, fmt.Errorf("decode %s: %w", what, err)
		}
		out = append(out, page...)
	}
	return out, nil
}

// Spec 读取 spec issue。GitHub 任何 issue 都可挂 sub-issues；
// 有 parent 时提示改用顶层 spec（parent 端点在 GHES 上 404，视为无父级）。
func (c *Client) Spec(ctx context.Context, num int) (Issue, error) {
	issue, err := c.Issue(ctx, c.repoID, num)
	if err != nil {
		return Issue{}, err
	}
	raw, err := c.api(ctx, fmt.Sprintf("repos/%s/issues/%d/parent", c.repoRef, num))
	if err == nil {
		var parent struct {
			Number int `json:"number"`
		}
		if json.Unmarshal(raw, &parent) == nil && parent.Number != 0 {
			return Issue{}, fmt.Errorf("issue %d is a sub-issue of #%d; pass the top-level spec issue", num, parent.Number)
		}
	} else if !isNotFound(err) {
		return Issue{}, fmt.Errorf("check parent of issue %d: %w", num, err)
	}
	return issue, nil
}

// Issue 读取指定仓库中的一个 issue。
func (c *Client) Issue(ctx context.Context, repoID, num int) (Issue, error) {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return Issue{}, err
	}
	raw, err := c.api(ctx, fmt.Sprintf("repos/%s/issues/%d", path, num))
	if err != nil {
		return Issue{}, fmt.Errorf("read issue %s#%d: %w", path, num, err)
	}
	var issue Issue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return Issue{}, fmt.Errorf("decode issue %s#%d: %w", path, num, err)
	}
	if issue.Number != num {
		return Issue{}, fmt.Errorf("issue %s#%d returned number %d", path, num, issue.Number)
	}
	return issue, nil
}

// Children 分页读取 spec 的全部 sub-issues（无 GitLab 式 type 过滤，
// 全部 sub-issue 都是工作项）。
func (c *Client) Children(ctx context.Context, specNum int) ([]RepoIssue, error) {
	raw, err := c.api(ctx, "--paginate", fmt.Sprintf("repos/%s/issues/%d/sub_issues", c.repoRef, specNum))
	if err != nil {
		return nil, fmt.Errorf("read sub_issues of #%d: %w", specNum, err)
	}
	issues, err := decodePages[Issue](raw, fmt.Sprintf("sub_issues of #%d", specNum))
	if err != nil {
		return nil, err
	}
	return c.bindAll(ctx, issues)
}

// BlockedBy 分页读取一个 issue 的 blocked-by 依赖（可跨仓库）。
// dependencies API 仅 github.com 支持；404 表示 GHES 无此 API，
// 返回空让调用方只靠正文 "Blocked by: #n" 降级解析。
func (c *Client) BlockedBy(ctx context.Context, repoID, num int) ([]RepoIssue, error) {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return nil, err
	}
	raw, err := c.api(ctx, "--paginate", fmt.Sprintf("repos/%s/issues/%d/dependencies/blocked_by", path, num))
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read blocked_by of %s#%d: %w", path, num, err)
	}
	issues, err := decodePages[Issue](raw, fmt.Sprintf("blocked_by of %s#%d", path, num))
	if err != nil {
		return nil, err
	}
	return c.bindAll(ctx, issues)
}

// bindAll 给一组 issue 绑定仓库 database id。
func (c *Client) bindAll(ctx context.Context, issues []Issue) ([]RepoIssue, error) {
	out := make([]RepoIssue, 0, len(issues))
	for _, issue := range issues {
		bound, err := c.bindRepo(ctx, issue)
		if err != nil {
			return nil, err
		}
		out = append(out, bound)
	}
	return out, nil
}

// IssueState 返回 issue 的开关、领取、占用与看板终态（claim 前重读与
// 周期刷新共用）。InFlight 表示看板 status 是进行中枚举且未指派给
// 当前用户——他人或别的 agent 正在处理，不应抢占；preview 未启用
// 时 status 为 null，InFlight 恒为 false、DoneOnBoard 恒为 true。
func (c *Client) IssueState(ctx context.Context, repoID, num int) (forge.IssueInfo, error) {
	iss, err := c.Issue(ctx, repoID, num)
	if err != nil {
		return forge.IssueInfo{}, err
	}
	return forge.IssueInfo{
		Open:            iss.IsOpen(),
		AssignedToMe:    iss.AssignedToMe(c.login),
		AssignedToOther: iss.AssignedToOther(c.login),
		InFlight:        iss.IsOpen() && iss.InFlight(c.login),
		DoneOnBoard:     iss.DoneOnBoard(),
	}, nil
}

// Claim 把 issue 以加法方式指派给当前用户（POST assignees）：既有
// 受理人不变，不替换他人指派。调用前必须重新读取状态与指派；
// 被他人领取的任务不得启动。
func (c *Client) Claim(ctx context.Context, repoID, num int) error {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return err
	}
	return c.apiJSON(ctx, map[string][]string{"assignees": {c.login}},
		"-X", "POST", fmt.Sprintf("repos/%s/issues/%d/assignees", path, num))
}

// IssueComments 按服务端顺序读出 issue 的全部评论。--paginate 的输出
// 可能是单个数组或多个连续数组，任一读取/解析失败都返回错误而不是空表。
func (c *Client) IssueComments(ctx context.Context, repoID, num int) ([]forge.IssueComment, error) {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return nil, err
	}
	raw, err := c.api(ctx, "--paginate", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", path, num))
	if err != nil {
		return nil, fmt.Errorf("read comments of %s#%d: %w", path, num, err)
	}
	var out []forge.IssueComment
	err = forge.DecodeArrays(raw, func(page []comment) error {
		for _, n := range page {
			created, perr := time.Parse(time.RFC3339, n.CreatedAt)
			if perr != nil {
				return fmt.Errorf("decode created_at %q of comment %d: %w", n.CreatedAt, n.ID, perr)
			}
			out = append(out, forge.IssueComment{
				ID:        n.ID,
				AuthorID:  n.User.ID,
				CreatedAt: created,
				Body:      n.Body,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read comments of %s#%d: %w", path, num, err)
	}
	return out, nil
}

// 默认看板状态名：GitHub 原生枚举值；GitLab 系统生命周期同名。
const (
	statusInProgress = "In progress"
	statusDone       = "Done"
)

// MarkInProgress 把 issue 的原生 status 置为 In progress（2026-11
// 公开预览功能）。preview 未启用时 status 参数被静默忽略——回读
// 响应验证生效，未生效报错由调用方降级为 warn。
func (c *Client) MarkInProgress(ctx context.Context, repoID, num int) error {
	return c.setStatus(ctx, repoID, num, statusInProgress)
}

// MarkDone 把 issue 的原生 status 置为 Done；status=Done 会自动
// 关闭 issue，调用方仍会照常执行 Close 兜底。
func (c *Client) MarkDone(ctx context.Context, repoID, num int) error {
	return c.setStatus(ctx, repoID, num, statusDone)
}

// setStatus 经 REST PATCH 设置 issue 的原生 status 并回读响应
// 验证确实生效。
func (c *Client) setStatus(ctx context.Context, repoID, num int, name string) error {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return err
	}
	raw, err := c.api(ctx, "-X", "PATCH",
		fmt.Sprintf("repos/%s/issues/%d", path, num),
		"-f", "status="+name)
	if err != nil {
		return err
	}
	var issue struct {
		Status *struct {
			Name string `json:"name"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &issue); err != nil {
		return fmt.Errorf("decode issue %s#%d after status update: %w", path, num, err)
	}
	if issue.Status == nil {
		return errors.New("issue status unsupported (preview not enabled?)")
	}
	if !strings.EqualFold(issue.Status.Name, name) {
		return fmt.Errorf("status of %s#%d is %q, want %q", path, num, issue.Status.Name, name)
	}
	return nil
}

// Close 关闭一个 issue。
func (c *Client) Close(ctx context.Context, repoID, num int) error {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return err
	}
	return c.apiJSON(ctx, nil, "-X", "PATCH", fmt.Sprintf("repos/%s/issues/%d", path, num), "-f", "state=closed")
}

// Note 在 issue 上追加一条评论。
func (c *Client) Note(ctx context.Context, repoID, num int, message string) error {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return err
	}
	_, err = c.api(ctx, "-X", "POST", fmt.Sprintf("repos/%s/issues/%d/comments", path, num),
		"-f", "body="+message)
	return err
}

// EnsureMR 确保 sourceBranch → targetBranch 的 PR 存在：
// 已有 open PR 返回其 html_url；否则经 REST 创建并返回 html_url。
func (c *Client) EnsureMR(ctx context.Context, repoID int, sourceBranch, targetBranch, title, description string) (string, error) {
	path, err := c.repoPath(ctx, repoID)
	if err != nil {
		return "", err
	}
	owner := strings.SplitN(path, "/", 2)[0]
	raw, err := c.api(ctx, fmt.Sprintf("repos/%s/pulls?state=open&head=%s:%s", path, owner, sourceBranch))
	if err != nil {
		return "", fmt.Errorf("list pull requests for %s: %w", sourceBranch, err)
	}
	var prs []struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &prs); err != nil {
		return "", fmt.Errorf("decode pull requests for %s: %w", sourceBranch, err)
	}
	if len(prs) > 0 && prs[0].HTMLURL != "" {
		return prs[0].HTMLURL, nil
	}
	raw, err = c.api(ctx, "-X", "POST", "repos/"+path+"/pulls",
		"-f", "head="+sourceBranch,
		"-f", "base="+targetBranch,
		"-f", "title="+title,
		"-f", "body="+description)
	if err != nil {
		return "", fmt.Errorf("create pull request %s -> %s: %w", sourceBranch, targetBranch, err)
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil || pr.HTMLURL == "" {
		return "", fmt.Errorf("decode created pull request for %s", sourceBranch)
	}
	return pr.HTMLURL, nil
}

// Login 返回当前 gh 登录用户。
func (c *Client) Login() string { return c.login }

// RepoID 返回 origin 仓库的 database id。
func (c *Client) RepoID() int { return c.repoID }
