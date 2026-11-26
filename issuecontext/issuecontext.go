// Package issuecontext 预取 issue 正文/评论，生成给 worker 的本地
// Markdown 快照，并控制注入 prompt 的截断与兼容逻辑。
package issuecontext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
)

// WriteResult 是一次快照写入的统计。
type WriteResult struct {
	// Files 是成功写入的 issue Markdown 文件数。
	Files int
	// CommentErrors 是评论预取失败但仍写出正文/元数据的 issue 数。
	CommentErrors int
}

type item struct {
	description string
	comments    []forge.IssueComment
	commentErr  error
}

// CommentReader 是 forge 客户端已有的评论读取能力。
type CommentReader interface {
	IssueComments(ctx context.Context, projectID, iid int) ([]forge.IssueComment, error)
}

// WrapSource 包装建图源：每次读取节点详情时并发读取该节点评论，
// 成功读到节点后把正文/评论存入 store。评论失败不会把依赖图构建
// 变成失败——正文仍可工作，快照会明确标注评论缺失。
func (s *Store) WrapSource(base graph.Source, comments CommentReader) graph.Source {
	return prefetchSource{Source: base, comments: comments, store: s}
}

type prefetchSource struct {
	graph.Source
	comments CommentReader
	store    *Store
}

func (s prefetchSource) Issue(ctx context.Context, key graph.Key) (graph.IssueInfo, error) {
	type commentResult struct {
		comments []forge.IssueComment
		err      error
	}
	result := make(chan commentResult, 1)
	go func() {
		if s.comments == nil {
			result <- commentResult{}
			return
		}
		comments, err := s.comments.IssueComments(ctx, key.ProjectID, key.IID)
		result <- commentResult{comments: comments, err: err}
	}()
	info, err := s.Source.Issue(ctx, key)
	res := <-result
	if err == nil {
		s.store.Put(key, info.Description, res.comments, res.err)
	}
	return info, err
}

// Store 是建图期间并发预取到的 issue 内容。graph fetch worker 会并发写
// 它，调度前再一次性物化成 Markdown 文件。
type Store struct {
	mu    sync.Mutex
	items map[graph.Key]item
}

// NewStore 创建空快照存储。
func NewStore() *Store {
	return &Store{items: make(map[graph.Key]item)}
}

// Put 保存一个 issue 的正文与评论。commentErr 非空表示评论预取失败；
// 正文仍可写快照，错误会在 Markdown 中明示，避免静默缺上下文。
func (s *Store) Put(key graph.Key, description string, comments []forge.IssueComment, commentErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[key] = item{description: description, comments: append([]forge.IssueComment(nil), comments...), commentErr: commentErr}
}

func (s *Store) get(key graph.Key) (item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[key]
	return it, ok
}

// WriteAll 把 store 中已成功读到的节点写成 `<projectID>-<IID>.md`。
// 目录按 0700、文件按 0600 处理：issue 内容可能含未公开需求，/tmp 是
// 多用户共享位置，不能默认放开读权限。
func (s *Store) WriteAll(dir string, g *graph.Graph) (WriteResult, error) {
	if err := prepareDir(dir); err != nil {
		return WriteResult{}, err
	}
	var result WriteResult
	for _, node := range g.Snapshot() {
		it, ok := s.get(node.Key)
		if !ok {
			continue
		}
		if it.commentErr != nil {
			result.CommentErrors++
		}
		text := renderMarkdown(node, it, time.Now())
		if err := writeFileAtomic(FilePath(dir, node.Key), text); err != nil {
			return result, err
		}
		result.Files++
	}
	return result, nil
}

// FilePath 返回一个 issue 的快照路径。projectID 是 forge 侧项目/仓库 id，
// 必须放进文件名：GitHub sub-issue 与 blocked-by 都可能跨仓库，单靠 IID 会撞名。
func FilePath(dir string, key graph.Key) string {
	return filepath.Join(dir, fmt.Sprintf("%d-%d.md", key.ProjectID, key.IID))
}

// Dir 返回项目 issue 快照目录。os.TempDir 在 Linux 默认是 /tmp；
// projectname 被清洗为单一路径组件，避免 forge 项目名里的分隔符或
// `..` 影响目录位置。
func Dir(projectName string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.':
			return r
		default:
			return '-'
		}
	}, projectName)
	clean = strings.Trim(clean, "-.")
	if clean == "" || strings.Trim(clean, ".") == "" {
		clean = "project"
	}
	return filepath.Join(os.TempDir(), clean+"-issues")
}

func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create issue context dir %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat issue context dir %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("issue context path %s is not a real directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("make issue context dir %s private: %w", dir, err)
		}
	}
	return nil
}

func writeFileAtomic(path, text string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".issue-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary issue context in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary issue context %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary issue context %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("protect temporary issue context %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace issue context %s: %w", path, err)
	}
	remove = false
	return nil
}

// claimNoise 匹配 fastimpl 跨机器认领协议评论。它们只服务调度仲裁，
// 对实现任务没有信息量。
var claimNoise = regexp.MustCompile(`^fastimpl-claim/v1\s+(claim|release)\s+\S+\s+\S+\s*$`)

func usefulComments(comments []forge.IssueComment) []forge.IssueComment {
	out := make([]forge.IssueComment, 0, len(comments))
	for _, c := range comments {
		body := strings.TrimSpace(c.Body)
		if body == "" || c.System || claimNoise.MatchString(body) || strings.HasPrefix(body, "fastimpl:") {
			continue
		}
		out = append(out, c)
	}
	return out
}

func renderMarkdown(node graph.Node, it item, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", node.Title)
	fmt.Fprintf(&b, "- Issue：`%s`\n", node.Key)
	fmt.Fprintf(&b, "- 类型：`%s`\n", fallback(node.Type, "issue"))
	state := "open"
	if node.Closed {
		state = "closed"
	}
	fmt.Fprintf(&b, "- 状态：`%s`\n", state)
	if len(node.Deps) == 0 {
		b.WriteString("- 前置依赖：无\n")
	} else {
		deps := make([]string, len(node.Deps))
		for i, dep := range node.Deps {
			deps[i] = "`" + dep.String() + "`"
		}
		fmt.Fprintf(&b, "- 前置依赖：%s\n", strings.Join(deps, "、"))
	}
	fmt.Fprintf(&b, "- 快照时间：`%s`\n", now.Format("2006-01-02 15:04:05 MST"))
	b.WriteString("\n> fastimpl 已过滤认领协议评论、运行状态备注与 forge 系统自动状态变更。\n")
	if it.commentErr != nil {
		fmt.Fprintf(&b, "\n> 注意：评论预取失败：%v。以下内容可能缺少评论。\n", it.commentErr)
	}
	b.WriteString("\n## 正文\n\n")
	if strings.TrimSpace(it.description) == "" {
		b.WriteString("（空）\n")
	} else {
		b.WriteString(strings.TrimSpace(it.description))
		b.WriteString("\n")
	}
	b.WriteString("\n## 评论\n\n")
	comments := usefulComments(it.comments)
	if len(comments) == 0 {
		b.WriteString("（无有效评论）\n")
		return b.String()
	}
	for _, c := range comments {
		fmt.Fprintf(&b, "### 评论 %d · %s\n\n%s\n\n", c.ID, c.CreatedAt.Format("2006-01-02 15:04:05 MST"), strings.TrimSpace(c.Body))
	}
	return b.String()
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// trimUTF8 把按字节截断后的串裁回完整 UTF-8 边界，避免 prompt 出现
// replacement rune。
func trimUTF8(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

const (
	// PromptContextLimit 是嵌入 prompt 的 issue Markdown 上限。完整
	// 快照不受这个上限影响；只有嵌入部分截断，避免巨型 issue/评论把
	// 首次 prompt 撑得过大。32KiB 对常规任务足够，同时有明确本地文件兜底。
	PromptContextLimit = 32 * 1024

	contextStartMarker = "<!-- fastimpl-issue-context-start -->"
	contextEndMarker   = "<!-- fastimpl-issue-context-end -->"
)

// PromptSection 把完整 Markdown 包装成可注入 prompt 的一段。path 总是
// 指出完整本地文件；超长内容按字节上限截到 UTF-8 边界，并用醒目说明
// 告诉 agent 需要遗漏部分时读本地文件，而不是先访问 forge。
func PromptSection(markdown, path string) string {
	content := strings.TrimSpace(markdown)
	if content == "" {
		return ""
	}
	truncated := false
	if len(content) > PromptContextLimit {
		content = trimUTF8(content[:PromptContextLimit])
		truncated = true
	}
	var b strings.Builder
	b.WriteString(contextStartMarker)
	b.WriteString("\n## fastimpl 预取的 issue 上下文\n\n")
	fmt.Fprintf(&b, "完整本地文件：`%s`。以下内容已经包含任务正文与过滤后的有效评论；不要先访问 forge 拉取正文/评论。\n\n", path)
	b.WriteString(content)
	if truncated {
		fmt.Fprintf(&b, "\n\n> 注意：issue 上下文超过 %d KiB，已在 prompt 中截断；完整内容在本地文件 `%s`。确实需要遗漏部分时读取该本地文件。\n", PromptContextLimit/1024, path)
	}
	b.WriteString("\n")
	b.WriteString(contextEndMarker)
	return b.String()
}

// EnsureInPrompt 确保渲染后的 prompt 含有上下文段。新模板可通过
// {{.IssueContext}} 自行放置；旧的定制/播种模板没有该变量时，把上下文
// 放到原 prompt 前面，同时移除旧默认模板中“读取任务正文和评论”的
// 指令，避免上下文已注入后 agent 仍先去 forge 慢速读取。
func EnsureInPrompt(rendered, section string) string {
	if strings.TrimSpace(section) == "" {
		return rendered
	}
	const oldReadInstruction = "读取任务正文和评论，先说明任务标题。"
	if strings.Contains(rendered, contextStartMarker) {
		// 新模板已经自行放置上下文；此时只移除标记外的旧读取指令。
		// 标记内文本是 issue 内容，即便巧合地包含同句话也不能改写。
		return replaceOutsideContext(rendered, oldReadInstruction, "先说明任务标题。")
	}
	if strings.Contains(rendered, oldReadInstruction) {
		// 旧默认模板把“读取正文/评论”放在工作目录/分支说明之后、其余
		// 操作要求之前。就地替换成上下文，避免 32KiB 快照把 implement
		// 与 worktree 等核心指令挤到 prompt 末尾。
		return strings.Replace(rendered, oldReadInstruction, strings.TrimSpace(section)+"\n\n先说明任务标题。", 1)
	}
	// 定制模板没有这个句子时无法推断最佳位置；放到前面仍比静默失效好，
	// 用户随后可用 {{.IssueContext}} 精确控制位置。
	return strings.TrimSpace(section) + "\n\n" + strings.TrimSpace(rendered) + "\n"
}

// replaceOutsideContext 只替换上下文标记区域外的第一处匹配。
// 这对混合模板很重要：模板可能既用了 {{.IssueContext}}，又残留旧式
// “读取任务正文和评论”指令；但 issue 正文/评论本身也可能巧合包含
// 同样的中文句子，标记内内容必须原样保留。
func replaceOutsideContext(rendered, old, replacement string) string {
	start := strings.Index(rendered, contextStartMarker)
	if start < 0 {
		return strings.Replace(rendered, old, replacement, 1)
	}
	end := strings.LastIndex(rendered, contextEndMarker)
	if end < start {
		return strings.Replace(rendered, old, replacement, 1)
	}
	end += len(contextEndMarker)
	before, middle, after := rendered[:start], rendered[start:end], rendered[end:]
	if strings.Contains(before, old) {
		before = strings.Replace(before, old, replacement, 1)
		return before + middle + after
	}
	if strings.Contains(after, old) {
		after = strings.Replace(after, old, replacement, 1)
	}
	return before + middle + after
}
