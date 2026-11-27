package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/forge"
)

// fakeRunner 记录每次 gh 调用并返回预设输出。
type fakeRunner struct {
	calls  [][]string
	stdins [][]byte
	out    map[string][]byte
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, dir string, argv []string, stdin []byte) ([]byte, error) {
	f.calls = append(f.calls, append([]string{}, argv...))
	f.stdins = append(f.stdins, append([]byte{}, stdin...))
	if f.err != nil {
		return nil, f.err
	}
	for k, v := range f.out {
		keys := strings.Fields(k)
		if len(argv) >= len(keys) && strings.Join(argv[:len(keys)], " ") == k {
			return v, nil
		}
	}
	return []byte("[]"), nil
}

// newTestClient 组装一个不探测网络的 Client。
func newTestClient(r Runner) *Client {
	return &Client{
		runner:    r,
		dir:       "/repo",
		host:      "github.example.com",
		repoRef:   "owner/name",
		repoID:    7,
		login:     "me",
		repoNames: map[int]string{7: "owner/name"},
		repoIDs:   map[string]int{"owner/name": 7},
	}
}

func TestEnsureMRIncludesDescription(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{
		"api --hostname github.example.com repos/owner/name/pulls?state=open&head=owner:integration": []byte("[]"),
		"api --hostname github.example.com -X POST repos/owner/name/pulls":                           []byte(`{"html_url":"https://github.example.com/owner/name/pull/1"}`),
	}}
	if _, err := newTestClient(r).EnsureMR(context.Background(), 7, "integration", "main", "spec #25 integration", "Close #25"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls[1], " ")
	if !strings.Contains(got, "-f body=Close #25") {
		t.Fatalf("create PR call = %q, want body description", got)
	}
}

// TestIssueCommentsPaginated 评论读取：--paginate 连续多个数组都要收，
// user.id 与 created_at 逐条解析。
func TestIssueCommentsPaginated(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{
		"api --hostname github.example.com --paginate repos/owner/name/issues/9/comments?per_page=100": []byte(
			`[{"id":21,"body":"first","created_at":"2024-02-03T04:05:06Z","user":{"id":201}}]` +
				`[{"id":22,"body":"second","created_at":"2024-02-03T04:05:07Z","user":{"id":202}}]`),
	}}
	c := newTestClient(r)
	comments, err := c.IssueComments(context.Background(), 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.IssueComment{
		{ID: 21, AuthorID: 201, CreatedAt: time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC), Body: "first"},
		{ID: 22, AuthorID: 202, CreatedAt: time.Date(2024, 2, 3, 4, 5, 7, 0, time.UTC), Body: "second"},
	}
	if len(comments) != len(want) {
		t.Fatalf("comments = %+v, want %d", comments, len(want))
	}
	for i := range want {
		if comments[i] != want[i] {
			t.Fatalf("comments[%d] = %+v, want %+v", i, comments[i], want[i])
		}
	}
}

// TestIssueCommentsErrors 读取失败与坏时间都返回错误而不是空表。
func TestIssueCommentsErrors(t *testing.T) {
	if _, err := newTestClient(&fakeRunner{err: errors.New("boom")}).IssueComments(context.Background(), 7, 9); err == nil {
		t.Fatal("read failure must surface as an error")
	}
	bad := &fakeRunner{out: map[string][]byte{
		"api --hostname github.example.com --paginate repos/owner/name/issues/9/comments?per_page=100": []byte(
			`[{"id":21,"body":"x","created_at":"nope","user":{"id":1}}]`),
	}}
	if _, err := newTestClient(bad).IssueComments(context.Background(), 7, 9); err == nil {
		t.Fatal("unparsable created_at must surface as an error")
	}
}

// TestClaimAdditiveViaREST 领取走 POST assignees，body 是显式 JSON 数组，
// 不是 -f 参数：-f 不会按数组语法转 JSON。
func TestClaimAdditiveViaREST(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{}}
	if err := newTestClient(r).Claim(context.Background(), 7, 9); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls[0], " ")
	want := "api --hostname github.example.com -X POST repos/owner/name/issues/9/assignees --input -"
	if got != want {
		t.Fatalf("claim call = %q, want %q", got, want)
	}
	var body struct {
		Assignees []string `json:"assignees"`
	}
	if err := json.Unmarshal(r.stdins[0], &body); err != nil {
		t.Fatalf("claim body %q is not JSON: %v", r.stdins[0], err)
	}
	if len(body.Assignees) != 1 || body.Assignees[0] != "me" {
		t.Fatalf("claim body = %q, want the real login in an array", r.stdins[0])
	}
}

// TestCloseViaRESTPatch 关闭走同 repo 的 PATCH state=closed：gh issue close
// 没有 --hostname 参数。
func TestCloseViaRESTPatch(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{}}
	if err := newTestClient(r).Close(context.Background(), 7, 9); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls[0], " ")
	want := "api --hostname github.example.com -X PATCH repos/owner/name/issues/9 -f state=closed"
	if got != want {
		t.Fatalf("close call = %q, want %q", got, want)
	}
	if strings.Contains(got, "issue close") {
		t.Fatalf("close must not use the issue subcommand: %q", got)
	}
}

// TestNoteViaREST 评论走 REST，不再给 issue comment 传 --hostname。
func TestNoteViaREST(t *testing.T) {
	r := &fakeRunner{out: map[string][]byte{}}
	if err := newTestClient(r).Note(context.Background(), 7, 9, "fastimpl-claim/v1 claim box-1 t1"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.calls[0], " ")
	want := "api --hostname github.example.com -X POST repos/owner/name/issues/9/comments -f body=fastimpl-claim/v1 claim box-1 t1"
	if got != want {
		t.Fatalf("note call = %q, want %q", got, want)
	}
	if strings.Contains(got, "issue comment") {
		t.Fatalf("note must not use the issue subcommand: %q", got)
	}
}

// TestAssignedToMeEqualFold 指派判定按登录名忽略大小写。
func TestAssignedToMeEqualFold(t *testing.T) {
	var iss Issue
	iss.Assignees = append(iss.Assignees, struct {
		Login string `json:"login"`
	}{Login: "ME"})
	if !iss.AssignedToMe("me") {
		t.Fatal("AssignedToMe should match case-insensitively")
	}
	if iss.AssignedToOther("me") {
		t.Fatal("AssignedToOther must not fire for the same login")
	}
	c := newTestClient(&fakeRunner{out: map[string][]byte{
		"api --hostname github.example.com repos/owner/name/issues/9": []byte(
			`{"number":9,"state":"open","assignees":[{"login":"ME"}]}`),
	}})
	info, err := c.IssueState(context.Background(), 7, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !info.AssignedToMe || info.AssignedToOther {
		t.Fatalf("info = %+v, want assigned to me", info)
	}
}
