package issuecontext

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
)

// Markdown 快照必须给 agent 有效上下文，同时过滤 fastimpl 自己的
// claim/release 协议评论和 forge 系统自动状态变更；文件按 projectID-IID
// 命名，避免跨项目 blocker 撞名。
func TestWriteAllFiltersNoiseAndWritesMarkdown(t *testing.T) {
	dir := t.TempDir()
	key := graph.Key{ProjectID: 1, IID: 9}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{
		Key: key, Title: "实现登录", Type: "task", Kind: graph.KindChild,
		Deps: []graph.Key{{ProjectID: 1, IID: 7}},
	})
	store := NewStore()
	store.Put(key, "正文第一行\n\n**不要漏掉边界**", []forge.IssueComment{
		{ID: 1, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Body: "fastimpl-claim/v1 claim box token"},
		{ID: 2, CreatedAt: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC), Body: "changed the status to In progress", System: true},
		{ID: 3, CreatedAt: time.Date(2026, 1, 2, 3, 4, 7, 0, time.UTC), Body: "补充：空 password 也要处理"},
		{ID: 4, CreatedAt: time.Date(2026, 1, 2, 3, 4, 8, 0, time.UTC), Body: "   \n\t"},
		{ID: 5, CreatedAt: time.Date(2026, 1, 2, 3, 4, 9, 0, time.UTC), Body: "fastimpl: worker failed\n排查终端: hc attach x"},
	}, nil)

	result, err := store.WriteAll(dir, g)
	if err != nil {
		t.Fatalf("WriteAll = %v", err)
	}
	if result.Files != 1 {
		t.Fatalf("WriteAll files = %d, want 1", result.Files)
	}
	path := filepath.Join(dir, "1-9.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written markdown: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"# 实现登录",
		"Issue：`1!9`",
		"类型：`task`",
		"前置依赖：`1!7`",
		"正文第一行",
		"**不要漏掉边界**",
		"补充：空 password 也要处理",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("markdown does not contain %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"fastimpl-claim/v1", "changed the status to In progress", "排查终端"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("markdown contains filtered noise %q:\n%s", unwanted, text)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat markdown: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("markdown mode = %o, want 600 because issue content may be private", got)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat context dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("context dir mode = %o, want 700", got)
	}
}

// prompt 里的快照需要控制体积：完整 Markdown 永远保留在本地文件，
// 超过上限时只截断嵌入内容并明确指向本地文件；注入还必须兼容仍含
// “读取任务正文和评论”的旧模板，避免 agent 又去 forge 慢速读取。
func TestPromptSectionTruncatesAndEnsureInPrompt(t *testing.T) {
	long := strings.Repeat("上下文边界", PromptContextLimit/len("上下文边界")+20)
	section := PromptSection("# 标题\n\n"+long, "/tmp/proj-issues/1-9.md")
	if !strings.Contains(section, contextStartMarker) || !strings.Contains(section, contextEndMarker) {
		t.Fatalf("section lacks stable start/end markers:\n%s", section)
	}
	if !strings.Contains(section, "完整本地文件：`/tmp/proj-issues/1-9.md`") {
		t.Fatalf("section lacks the local file path:\n%s", section)
	}
	if !strings.Contains(section, "已在 prompt 中截断") {
		t.Fatalf("long section lacks the truncation notice")
	}
	if strings.Contains(section, long) {
		t.Fatal("section still contains the untruncated issue body")
	}
	if !utf8.ValidString(section) {
		t.Fatal("truncated section is not valid UTF-8")
	}

	rendered := "使用 implement 技能。\n读取任务正文和评论，先说明任务标题。只在 worktree 修改。"
	out := EnsureInPrompt(rendered, section)
	if strings.Contains(out, "读取任务正文和评论") {
		t.Fatalf("old template instruction survives injection:\n%s", out)
	}
	if !strings.Contains(out, "先说明任务标题。只在 worktree 修改。") {
		t.Fatalf("original prompt instructions were lost:\n%s", out)
	}
	if strings.Count(out, contextStartMarker) != 1 {
		t.Fatalf("injected context count != 1:\n%s", out)
	}
	if strings.Index(out, "使用 implement 技能。") > strings.Index(out, contextStartMarker) {
		t.Fatalf("context should replace the old read instruction in place, not push the core operating instructions after a large snapshot:\n%s", out)
	}
	if again := EnsureInPrompt(out, section); again != out {
		t.Fatal("EnsureInPrompt is not idempotent")
	}

	// issue 正文本身可能巧合地包含旧句式。新模板已经放置上下文时，
	// 只能替换上下文标记外面的旧指令，绝不能改写 issue 内容。
	embedded := strings.Replace(section, "# 标题", "# 标题\n\n读取任务正文和评论，先说明任务标题。", 1)
	mixed := "使用 implement 技能。\n" + embedded + "\n读取任务正文和评论，先说明任务标题。\n只在 worktree 修改。"
	fixed := EnsureInPrompt(mixed, embedded)
	end := strings.Index(fixed, contextEndMarker) + len(contextEndMarker)
	if end < len(contextEndMarker) {
		t.Fatalf("fixed prompt lost end marker:\n%s", fixed)
	}
	if strings.Contains(fixed[end:], "读取任务正文和评论") {
		t.Fatalf("old instruction outside the embedded context was not replaced:\n%s", fixed[end:])
	}
	inner := fixed[:end]
	if !strings.Contains(inner, "# 标题\n\n读取任务正文和评论，先说明任务标题。") {
		t.Fatalf("issue content inside context markers was rewritten:\n%s", inner)
	}
}

// 建图读取每个节点时，评论预取必须与节点读取同阶段发生：建图结束后
// store 已经能用正文+有效评论写快照，不需要任务启动时再访问 forge。
func TestStoreWrapSourcePrefetchesCommentsDuringGraphBuild(t *testing.T) {
	key := graph.Key{ProjectID: 1, IID: 9}
	source := &fakeGraphSource{
		spec:     graph.IssueInfo{Key: graph.Key{ProjectID: 1, IID: 8}, Title: "spec", Type: "issue", Open: true},
		children: []graph.ChildRef{{Key: key, Title: "task", State: "opened", Type: "task"}},
		issues: map[graph.Key]graph.IssueInfo{
			key: {Key: key, Title: "task", Type: "task", Open: true, Description: "任务正文"},
		},
	}
	comments := &fakeCommentReader{byKey: map[graph.Key][]forge.IssueComment{
		key: {{ID: 31, CreatedAt: time.Date(2026, 1, 3, 4, 5, 6, 0, time.UTC), Body: "评论里的验收条件"}},
	}}
	store := NewStore()
	g, err := graph.Build(t.Context(), store.WrapSource(source, comments), 8)
	if err != nil {
		t.Fatalf("Build = %v", err)
	}
	if got := comments.calls(key); got != 1 {
		t.Fatalf("comment reads for %s = %d, want 1", key, got)
	}
	dir := t.TempDir()
	if _, err := store.WriteAll(dir, g); err != nil {
		t.Fatalf("WriteAll = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "1-9.md"))
	if err != nil {
		t.Fatalf("read context: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "任务正文") || !strings.Contains(text, "评论里的验收条件") {
		t.Fatalf("prefetched markdown lacks body/comment:\n%s", text)
	}
}

type fakeGraphSource struct {
	spec     graph.IssueInfo
	children []graph.ChildRef
	issues   map[graph.Key]graph.IssueInfo
}

func (f *fakeGraphSource) Spec(context.Context, int) (graph.IssueInfo, error) { return f.spec, nil }
func (f *fakeGraphSource) Children(context.Context, int) ([]graph.ChildRef, error) {
	return f.children, nil
}
func (f *fakeGraphSource) BlockedBy(context.Context, graph.Key) ([]graph.Key, error) { return nil, nil }
func (f *fakeGraphSource) Issue(_ context.Context, key graph.Key) (graph.IssueInfo, error) {
	return f.issues[key], nil
}

type fakeCommentReader struct {
	mu     sync.Mutex
	byKey  map[graph.Key][]forge.IssueComment
	errs   map[graph.Key]error
	counts map[graph.Key]int
}

func (f *fakeCommentReader) IssueComments(_ context.Context, projectID, iid int) ([]forge.IssueComment, error) {
	key := graph.Key{ProjectID: projectID, IID: iid}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts == nil {
		f.counts = make(map[graph.Key]int)
	}
	f.counts[key]++
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	return append([]forge.IssueComment(nil), f.byKey[key]...), nil
}

func (f *fakeCommentReader) calls(key graph.Key) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[key]
}

// `/tmp/{projectname}-issues` 的 projectname 来自 forge 项目路径，
// 必须清洗成单一路径组件；否则恶意/异常项目名可能逃逸出临时目录。
func TestDirSanitizesProjectName(t *testing.T) {
	if got, want := filepath.Base(Dir("bf9100")), "bf9100-issues"; got != want {
		t.Fatalf("Dir basename = %q, want %q", got, want)
	}
	got := filepath.Base(Dir("../bad/name"))
	if strings.Contains(got, "/") || strings.Contains(got, `\\`) || strings.HasPrefix(got, "..") {
		t.Fatalf("unsafe Dir basename %q", got)
	}
	if got := filepath.Base(Dir("..")); got != "project-issues" {
		t.Fatalf("Dir(%q) basename = %q, want project-issues", "..", got)
	}
}

// 评论预取失败是加速路径的失败，不应让依赖图/任务调度失败；快照仍写
// 正文和元数据，并明确标注评论缺失，agent 能判断是否真的需要补查。
func TestCommentPrefetchFailureDegradesButDoesNotFailGraph(t *testing.T) {
	key := graph.Key{ProjectID: 1, IID: 9}
	source := &fakeGraphSource{
		spec:     graph.IssueInfo{Key: graph.Key{ProjectID: 1, IID: 8}, Title: "spec", Type: "issue", Open: true},
		children: []graph.ChildRef{{Key: key, Title: "task", State: "opened", Type: "task"}},
		issues:   map[graph.Key]graph.IssueInfo{key: {Key: key, Title: "task", Type: "task", Open: true, Description: "正文仍在"}},
	}
	comments := &fakeCommentReader{errs: map[graph.Key]error{key: errors.New("forge notes unavailable")}}
	store := NewStore()
	g, err := graph.Build(t.Context(), store.WrapSource(source, comments), 8)
	if err != nil {
		t.Fatalf("Build with comment prefetch failure = %v", err)
	}
	dir := t.TempDir()
	result, err := store.WriteAll(dir, g)
	if err != nil {
		t.Fatalf("WriteAll = %v", err)
	}
	if result.CommentErrors != 1 {
		t.Fatalf("CommentErrors = %d, want 1", result.CommentErrors)
	}
	data, err := os.ReadFile(filepath.Join(dir, "1-9.md"))
	if err != nil {
		t.Fatalf("read context: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "正文仍在") || !strings.Contains(text, "评论预取失败") {
		t.Fatalf("degraded markdown lacks body/error notice:\n%s", text)
	}
}
