package gitlab

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/forge"
)

// fakeRunner 记录每次 glab 调用并返回预设输出。
type fakeRunner struct {
	calls  [][]string
	out    map[string][]byte
	err    error
	prefix string // 只按参数前缀匹配 out 的键
}

func (f *fakeRunner) Run(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, append([]string{}, argv...))
	if f.err != nil {
		return nil, f.err
	}
	for k, v := range f.out {
		if len(argv) >= len(strings.Fields(k)) && strings.Join(argv[:len(strings.Fields(k))], " ") == k {
			return v, nil
		}
	}
	return []byte("[]"), nil
}

// newTestClient 组装一个不探测网络的 Client。
func newTestClient(r Runner) *Client {
	return &Client{
		runner:      r,
		dir:         "/repo",
		host:        "gitlab.example.com",
		repoRef:     "gitlab.example.com/group/proj",
		projectPath: "group/proj",
		projectID:   42,
		username:    "me",
		repoRefs:    map[int]string{},
	}
}

// TestIssueCommentsPaginated 评论读取：--paginate 连续多个数组都要收，
// author.id 与 created_at 逐条解析。
func TestIssueCommentsPaginated(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{
		"api --hostname gitlab.example.com --paginate projects/group%2Fproj/issues/7/notes?per_page=100": []byte(
			`[{"id":11,"body":"first","created_at":"2024-01-02T03:04:05Z","author":{"id":101}}]` +
				`[{"id":12,"body":"second","created_at":"2024-01-02T03:04:06Z","system":true,"author":{"id":102}}]`),
	}}
	c := newTestClient(r)
	comments, err := c.IssueComments(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.IssueComment{
		{ID: 11, AuthorID: 101, CreatedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), Body: "first"},
		{ID: 12, AuthorID: 102, CreatedAt: time.Date(2024, 1, 2, 3, 4, 6, 0, time.UTC), Body: "second", System: true},
	}
	if len(comments) != len(want) {
		t.Fatalf("comments = %+v, want %d", comments, len(want))
	}
	for i := range want {
		if comments[i] != want[i] {
			t.Fatalf("comments[%d] = %+v, want %+v", i, comments[i], want[i])
		}
	}
	if len(r.calls) != 1 || strings.Join(r.calls[0], " ") !=
		"api --hostname gitlab.example.com --paginate projects/group%2Fproj/issues/7/notes?per_page=100" {
		t.Fatalf("glab calls = %v", r.calls)
	}
}

// TestIssueCommentsSingleArrayAndErrors 单个数组也能解析；读取失败与坏
// 时间都返回错误而不是空表。
func TestIssueCommentsSingleArrayAndErrors(t *testing.T) {
	c := newTestClient(&fakeRunner{out: map[string][]byte{
		"api --hostname gitlab.example.com --paginate projects/group%2Fproj/issues/8/notes?per_page=100": []byte(
			`[{"id":1,"body":"x","created_at":"2024-05-06T07:08:09Z","author":{"id":5}}]`),
	}})
	comments, err := c.IssueComments(context.Background(), 42, 8)
	if err != nil || len(comments) != 1 || comments[0].AuthorID != 5 {
		t.Fatalf("comments = %+v err = %v", comments, err)
	}

	c = newTestClient(&fakeRunner{err: errors.New("boom")})
	if _, err := c.IssueComments(context.Background(), 42, 8); err == nil {
		t.Fatal("read failure must surface as an error")
	}

	c = newTestClient(&fakeRunner{out: map[string][]byte{
		"api --hostname gitlab.example.com --paginate projects/group%2Fproj/issues/9/notes?per_page=100": []byte(
			`[{"id":1,"body":"x","created_at":"not-a-time","author":{"id":5}}]`),
	}})
	if _, err := c.IssueComments(context.Background(), 42, 9); err == nil {
		t.Fatal("unparsable created_at must surface as an error")
	}
}

// TestClaimAdditive 领取用 --assignee +<username>（加法），不替换既有受理人。
func TestClaimAdditive(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{}}
	c := newTestClient(r)
	if err := c.Claim(context.Background(), 42, 7); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls[0], " ")
	if !strings.Contains(got, "issue update 7") || !strings.Contains(got, "--assignee +me") {
		t.Fatalf("claim call = %q", got)
	}
}

// TestIssueStateAssignedToMe GraphQL 应答里的受理人：本人 → AssignedToMe，
// 他人 → AssignedToOther。
func TestIssueStateAssignedToMe(t *testing.T) {
	reply := func(users string) []byte {
		return []byte(`{"data":{"project":{"workItems":{"nodes":[{"state":"OPEN","widgets":[` +
			`{"__typename":"WorkItemWidgetAssignees","assignees":{"nodes":[` + users + `]}}]}]}}}}`)
	}
	r := &fakeRunner{out: map[string][]byte{
		"api --hostname gitlab.example.com --input -": reply(`{"username":"me"}`),
	}}
	c := newTestClient(r)
	info, err := c.IssueState(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Open || !info.AssignedToMe || info.AssignedToOther {
		t.Fatalf("info = %+v, want open and assigned to me", info)
	}

	r = &fakeRunner{out: map[string][]byte{
		"api --hostname gitlab.example.com --input -": reply(`{"username":"someone-else"}`),
	}}
	c = newTestClient(r)
	info, err = c.IssueState(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if info.AssignedToMe || !info.AssignedToOther {
		t.Fatalf("info = %+v, want assigned to other only", info)
	}
}
