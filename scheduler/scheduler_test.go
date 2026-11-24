package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// fakeForge 记录 Claim/Close/Note/状态标记调用；closedOn/other 控制
// IssueState；markErr 让状态标记失败，验证 warn-only 路径。
// 评论按服务端模型保存（递增 ID、递增时间、按作者的 authorID），
// 用于验证跨机器认领协议。
type fakeForge struct {
	mu       sync.Mutex
	claimed  []graph.Key
	doing    []graph.Key
	done     []graph.Key
	closed   []graph.Key
	notes    []string
	comments map[int][]forge.IssueComment // issue IID → 评论
	nextID   int64
	// commentCalls 记录 IssueComments 的调用次数（barrier 用）。
	commentCalls int
	clock        time.Time
	closedOn     map[int]bool // IssueState 返回 open=false
	other        map[int]bool // IssueState 返回 assignedToOther=true
	inFlight     map[int]bool // IssueState 返回 inFlight=true（他人处理中）
	boardDirty   map[int]bool // IssueState 返回 doneOnBoard=false（closed 但看板未终态）
	markErr      map[int]bool // MarkInProgress/MarkDone 返回错误
	myLogin      string       // Claim 后 AssignedToMe 判定用
	authorID     int64        // Note 写入的评论作者 ID
	// commentErr 让 IssueComments 报错；assigneeLost 让 Claim 后回读
	// 仍不是本人（模拟指派没生效）；otherAfterClaim/closedAfterClaim
	// 让 Claim 之后的状态回读变成他人领取/已关闭。
	commentErr       error
	assigneeLost     bool
	claimNoteErr     error
	otherAfterClaim  map[int]bool
	closedAfterClaim map[int]bool
	// stateErr/stateErrAt 让第 stateErrAt 次 IssueState 调用报错。
	stateErr   error
	stateErrAt int
	stateCalls int
	// claimBarrier 在读完评论后、返回前被调用（带上第几次读取与本次
	// 读到的 claim 标记数），用来让两个实例在同一时刻竞争；nil 表示不等。
	claimBarrier func(call, claims int)
	// onComment 在每次写入评论后回调（用于让对手在看到标记后行动）。
	onComment func(forge.IssueComment)
}

// IssueState 默认全部 open、未领取未占用、看板终态一致；
// closedOn/other/inFlight/boardDirty 覆盖；Claim 会把 issue 记成本人
// 指派（除非 assigneeLost）。
func (f *fakeForge) IssueState(ctx context.Context, projectID, iid int) (forge.IssueInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateCalls++
	if f.stateErr != nil && f.stateCalls == f.stateErrAt {
		return forge.IssueInfo{}, f.stateErr
	}
	claimed := f.assignedToMe(iid)
	other := f.other[iid]
	closed := f.closedOn[iid]
	if claimed {
		other = other || f.otherAfterClaim[iid]
		closed = closed || f.closedAfterClaim[iid]
	}
	return forge.IssueInfo{
		Open:            !closed,
		AssignedToMe:    claimed,
		AssignedToOther: other,
		InFlight:        f.inFlight[iid],
		DoneOnBoard:     !f.boardDirty[iid],
	}, nil
}

// assignedToMe 报告当前 fake 里 issue 是否指派给了本人。
func (f *fakeForge) assignedToMe(iid int) bool {
	if f.assigneeLost {
		return false
	}
	for _, k := range f.claimed {
		if k.IID == iid {
			return true
		}
	}
	return false
}

// IssueComments 返回当前全部评论（含服务端 ID/时间/author）。
func (f *fakeForge) IssueComments(ctx context.Context, projectID, iid int) ([]forge.IssueComment, error) {
	f.mu.Lock()
	barrier := f.claimBarrier
	err := f.commentErr
	out := append([]forge.IssueComment{}, f.comments[iid]...)
	f.commentCalls++
	call := f.commentCalls
	f.mu.Unlock()
	if barrier != nil {
		barrier(call, countClaims(out))
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// countClaims 数出评论里 claim 标记的条数。
func countClaims(comments []forge.IssueComment) int {
	n := 0
	for _, c := range comments {
		if kind, _, _, ok := parseClaim(c.Body); ok && kind == claimKindYes {
			n++
		}
	}
	return n
}

// addComment 以服务端身份在 iid 上追加一条评论，返回它。
func (f *fakeForge) addComment(iid int, body string, authorID int64) forge.IssueComment {
	f.nextID++
	if f.clock.IsZero() {
		f.clock = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	f.clock = f.clock.Add(time.Second)
	if f.comments == nil {
		f.comments = map[int][]forge.IssueComment{}
	}
	c := forge.IssueComment{ID: f.nextID, AuthorID: authorID, CreatedAt: f.clock, Body: body}
	f.comments[iid] = append(f.comments[iid], c)
	return c
}

func (f *fakeForge) Claim(ctx context.Context, projectID, iid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimed = append(f.claimed, graph.Key{ProjectID: projectID, IID: iid})
	return nil
}

func (f *fakeForge) MarkInProgress(ctx context.Context, projectID, iid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr[iid] {
		return fmt.Errorf("status unsupported")
	}
	f.doing = append(f.doing, graph.Key{ProjectID: projectID, IID: iid})
	return nil
}

func (f *fakeForge) MarkDone(ctx context.Context, projectID, iid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr[iid] {
		return fmt.Errorf("status unsupported")
	}
	f.done = append(f.done, graph.Key{ProjectID: projectID, IID: iid})
	return nil
}

func (f *fakeForge) Close(ctx context.Context, projectID, iid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, graph.Key{ProjectID: projectID, IID: iid})
	return nil
}

func (f *fakeForge) Note(ctx context.Context, projectID, iid int, message string) error {
	f.mu.Lock()
	if f.claimNoteErr != nil && strings.HasPrefix(message, claimVersion) {
		err := f.claimNoteErr
		f.mu.Unlock()
		return err
	}
	f.notes = append(f.notes, fmt.Sprintf("%d!%d: %s", projectID, iid, message))
	author := f.authorID
	if author == 0 {
		author = 100
	}
	c := f.addComment(iid, message, author)
	hook := f.onComment
	f.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	return nil
}

func (f *fakeForge) EnsureMR(ctx context.Context, projectID int, sourceBranch, targetBranch, title string) (string, error) {
	return "http://mr/1", nil
}

// fakeSessions 假 hc 会话：记录启动参数；从提示词里提取令牌，
// ReadScreen 返回对应的报告帧。
type fakeSessions struct {
	mu      sync.Mutex
	started []worker.Params
	nextID  int
	// 测试用模板 "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}"，从渲染结果取令牌。
	tokens map[string]string
	// startErr/stopErr 让启动/停止失败；stops 记录停过的地址。
	startErr error
	stopErr  error
	stops    []string
}

func (f *fakeSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.nextID++
	f.started = append(f.started, p)
	return fmt.Sprintf("t%d", f.nextID), nil
}

func (f *fakeSessions) Snapshot(ctx context.Context, addr string) (worker.Record, error) {
	return worker.Record{Live: true, Status: "idle", Reported: true}, nil
}

func (f *fakeSessions) ReadScreen(ctx context.Context, addr string, lines int) (string, bool, error) {
	f.mu.Lock()
	token := f.tokens[addr]
	f.mu.Unlock()
	return fmt.Sprintf("some output\nFASTIMPL-READY %s\ndone: implemented and verified\n", token), true, nil
}

func (f *fakeSessions) Stop(ctx context.Context, addr string, specIID int, taskName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stops = append(f.stops, addr)
	return nil
}

func (f *fakeSessions) AwaitBoot(ctx context.Context, addr string) (worker.Record, error) {
	return worker.Record{Live: true, Status: "idle", Reported: true}, nil
}

func (f *fakeSessions) Prompt(ctx context.Context, addr, text string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens == nil {
		f.tokens = map[string]string{}
	}
	// 续写/冲突提示词不含模板分隔符，从提示词里的
	// "FASTIMPL-READY <token>" 按令牌格式提取（token 后可能紧跟
	// 中文标点，不能用空白分词）；都没有则沿用已记录令牌。
	if parts := strings.Split(text, "|"); len(parts) > 1 {
		f.tokens[addr] = parts[1]
	} else if m := readyTokenRE.FindStringSubmatch(text); m != nil {
		f.tokens[addr] = m[1]
	}
	return 0, nil
}

// readyTokenRE 从提示词中提取报告令牌："FASTIMPL-READY <t+hex>"。
var readyTokenRE = regexp.MustCompile(worker.ReadyMarker + `\s+(t[0-9a-f]+)`)

func (f *fakeSessions) WaitTurn(ctx context.Context, addr string, baseline uint64, budget time.Duration) (worker.Record, error) {
	return worker.Record{Turn: baseline + 1, Live: true, Status: "done"}, nil
}

// testInstanceID 是 scheduler 测试使用的稳定实例 ID。
const testInstanceID = "test-box-1"

// newTestScheduler 同 New，但把发 claim 后的等待与运行期复查间隔
// 压到零/极短，测试不引入真实网络与真实等待。
func newTestScheduler(cfg config.Config, repo *worktree.Repo, f forge.Forge, forgeName string, g *graph.Graph, sessions Sessions, template, workDir, intDir, intBranch, baseName string, agentArgv []string, log *slog.Logger) *Scheduler {
	s := New(cfg, repo, f, forgeName, g, sessions, template, workDir, intDir, intBranch, baseName, agentArgv, log)
	s.claimWait = 0
	s.claimRecheck = 20 * time.Millisecond
	return s
}

// initRepo 建一个带初始提交的临时 git 仓库。
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	// 裸仓库作 origin：集成分支最终要 push。
	bare := filepath.Join(t.TempDir(), "origin.git")
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	run("remote", "add", "origin", bare)
	run("push", "-u", "origin", "main")
	return dir
}

func TestSchedulerRun(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// 图：#9 无前置、#10 依赖 #9；#11 在 forge 上已被外部关闭，
	// 开跑前重读应直接跳过、不 claim 不建 worker。
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 10}, Title: "task B", Kind: graph.KindChild, Deps: []graph.Key{{ProjectID: 1, IID: 9}}})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 11}, Title: "closed task", Kind: graph.KindChild})

	forge := &fakeForge{closedOn: map[int]bool{11: true}, markErr: map[int]bool{10: true}}
	sessions := &fakeSessions{}
	// worker 在各自 worktree 里制造一次提交（Merge 需要真实差异）。
	hookSessions := &commitSessions{fakeSessions: sessions, t: t}
	cfg := config.Config{Parallelism: 4, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, forge, "gitlab", g, hookSessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !g.Done() {
		t.Fatal("graph not done")
	}
	// 顺序：#9 必须先于 #10 被 claim；#11 不 claim。
	forge.mu.Lock()
	claimed := append([]graph.Key{}, forge.claimed...)
	closed := append([]graph.Key{}, forge.closed...)
	forge.mu.Unlock()
	if len(claimed) != 2 || claimed[0].IID != 9 || claimed[1].IID != 10 {
		t.Fatalf("claimed %v, want [9 10] in order", claimed)
	}
	if len(closed) != 2 {
		t.Fatalf("closed %v, want 2 issues", closed)
	}
	// 状态标记：#9 记录 In progress→Done；#10 标记失败只 warn，
	// 不阻塞领取/集成/关闭。
	forge.mu.Lock()
	doing := append([]graph.Key{}, forge.doing...)
	done := append([]graph.Key{}, forge.done...)
	forge.mu.Unlock()
	if len(doing) != 1 || doing[0].IID != 9 {
		t.Fatalf("doing marks %v, want [9] (#10 fails → warn only)", doing)
	}
	if len(done) != 1 || done[0].IID != 9 {
		t.Fatalf("done marks %v, want [9] (#10 fails → warn only)", done)
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 2 {
		t.Fatalf("workers started = %d, want 2 (#11 must not start)", started)
	}
	// 集成 worktree 应包含两个任务的合并提交与文件。
	for _, f := range []string{"task9.txt", "task10.txt"} {
		if _, err := os.Stat(filepath.Join(intDir, f)); err != nil {
			t.Errorf("integration dir missing %s: %v", f, err)
		}
	}
}

// commitSessions 在 StartWorker 之后往该任务 worktree 提交一个文件，
// 模拟 worker 的真实产出，让 Merge 有内容可合。
type commitSessions struct {
	*fakeSessions
	t *testing.T
}

func (c *commitSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	addr, err := c.fakeSessions.StartWorker(ctx, p)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("task%d.txt", p.IssueIID)
	file := filepath.Join(p.Worktree, name)
	if err := os.WriteFile(file, []byte(name+"\n"), 0o644); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"add", name}, {"commit", "-m", "task " + name}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = p.Worktree
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %v\n%s", args, err, out)
		}
	}
	return addr, nil
}

// conflictSessions 让两个任务都修改同一个文件制造合并冲突；
// 收到冲突解决提示词时在任务 worktree 里执行真实的
// merge+解冲突+提交，模拟 agent 解决冲突的行为。
type conflictSessions struct {
	*fakeSessions
	intBranch   string
	mu          sync.Mutex
	worktrees   map[string]string // addr -> 任务 worktree
	conflicts   int               // 收到的冲突提示词数
	startedN    int
	bothStarted chan struct{}
}

func (c *conflictSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	addr, err := c.fakeSessions.StartWorker(ctx, p)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.worktrees == nil {
		c.worktrees = map[string]string{}
	}
	if c.bothStarted == nil {
		c.bothStarted = make(chan struct{})
	}
	ch := c.bothStarted
	c.worktrees[addr] = p.Worktree
	c.startedN++
	if c.startedN == 2 {
		close(ch)
	}
	c.mu.Unlock()
	// 等两个任务的分支都从同一基点建好再提交，
	// 否则后建的分支会带上先合入的内容而不再冲突。
	select {
	case <-ch:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(10 * time.Second):
		return "", fmt.Errorf("timed out waiting for both workers")
	}
	file := filepath.Join(p.Worktree, "conflict.txt")
	if err := os.WriteFile(file, []byte(fmt.Sprintf("task %d\n", p.IssueIID)), 0o644); err != nil {
		return "", err
	}
	if !git(p.Worktree, "add", "conflict.txt") || !git(p.Worktree, "commit", "-m", fmt.Sprintf("task %d conflict", p.IssueIID)) {
		return "", fmt.Errorf("commit conflict.txt in %s failed", p.Worktree)
	}
	return addr, nil
}

// Prompt 记录提示词；冲突提示词触发真实的 merge 解冲突提交。
func (c *conflictSessions) Prompt(ctx context.Context, addr, text string) (uint64, error) {
	n, err := c.fakeSessions.Prompt(ctx, addr, text)
	if err != nil {
		return n, err
	}
	if !strings.Contains(text, "git merge") {
		return n, nil
	}
	c.mu.Lock()
	c.conflicts++
	dir := c.worktrees[addr]
	c.mu.Unlock()
	if dir == "" {
		return n, fmt.Errorf("no worktree for %s", addr)
	}
	// 模拟 agent：merge 集成分支 → 冲突 → 写成合并结果 → 提交。
	_ = git(dir, "merge", c.intBranch) // 预期冲突，忽略退出码
	if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte("resolved\n"), 0o644); err != nil {
		return n, err
	}
	if !git(dir, "add", "conflict.txt") || !git(dir, "commit", "-m", "resolve conflict") {
		return n, fmt.Errorf("resolve conflict in %s failed", dir)
	}
	return n, nil
}

// git 在 dir 执行一条 git 命令，返回是否成功。
func git(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// baseSyncSessions 模拟集成分支专属 agent：普通任务 worker 启动时
// 在任务 worktree 提交一个文件；收到"git merge origin/<base>"提示词时
// 在集成 worktree 里真实执行 merge、解冲突、提交。
type baseSyncSessions struct {
	*fakeSessions
	mu        sync.Mutex
	worktrees map[string]string
	names     []string
	resolved  int
}

func (b *baseSyncSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	addr, err := b.fakeSessions.StartWorker(ctx, p)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	if b.worktrees == nil {
		b.worktrees = map[string]string{}
	}
	b.worktrees[addr] = p.Worktree
	b.names = append(b.names, p.Name)
	b.mu.Unlock()
	if strings.HasSuffix(p.Name, "-integration") {
		return addr, nil // 集成 agent 不写任务文件
	}
	file := filepath.Join(p.Worktree, "task9.txt")
	if err := os.WriteFile(file, []byte("task9\n"), 0o644); err != nil {
		return "", err
	}
	if !git(p.Worktree, "add", "task9.txt") || !git(p.Worktree, "commit", "-m", "task 9") {
		return "", fmt.Errorf("commit task file in %s failed", p.Worktree)
	}
	return addr, nil
}

// Prompt 记录提示词；base 冲突提示词触发真实的 merge 解冲突提交。
func (b *baseSyncSessions) Prompt(ctx context.Context, addr, text string) (uint64, error) {
	n, err := b.fakeSessions.Prompt(ctx, addr, text)
	if err != nil {
		return n, err
	}
	if !strings.Contains(text, "git merge origin/") {
		return n, nil
	}
	b.mu.Lock()
	b.resolved++
	dir := b.worktrees[addr]
	b.mu.Unlock()
	if dir == "" {
		return n, fmt.Errorf("no worktree for %s", addr)
	}
	// 模拟集成 agent：merge origin/main → 冲突 → 写成解决结果 → 提交。
	_ = git(dir, "merge", "origin/main") // 预期冲突，忽略退出码
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("resolved\n"), 0o644); err != nil {
		return n, err
	}
	if !git(dir, "add", "base.txt") || !git(dir, "commit", "-m", "resolve base conflict") {
		return n, fmt.Errorf("resolve base conflict in %s failed", dir)
	}
	return n, nil
}

// TestSchedulerBaseSyncConflict 上游 main 在集成分支分叉后被推进，
// SyncBase 冲突 → 调度器启动集成专属 agent 在集成 worktree 解决 →
// 验证干净后任务分支照常合并。
func TestSchedulerBaseSyncConflict(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// 集成分支已有自己的提交（模拟之前集成过的任务改了 base.txt）。
	if err := os.WriteFile(filepath.Join(intDir, "base.txt"), []byte("integration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !git(intDir, "commit", "-am", "integrated change") {
		t.Fatal("commit integration divergence")
	}
	// 上游在主 checkout 里把 main 推进一步并改同一文件，制造 base 冲突。
	if err := os.WriteFile(filepath.Join(repoDir, "base.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !git(repoDir, "commit", "-am", "upstream change") || !git(repoDir, "push", "origin", "main") {
		t.Fatal("push upstream change")
	}

	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})

	sessions := &baseSyncSessions{fakeSessions: &fakeSessions{}}
	cfg := config.Config{Parallelism: 1, SpecIID: 8, Retries: 2, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, &fakeForge{}, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !g.Done() {
		t.Fatal("graph not done")
	}
	sessions.mu.Lock()
	resolved, names := sessions.resolved, append([]string{}, sessions.names...)
	sessions.mu.Unlock()
	if resolved != 1 {
		t.Fatalf("base conflict resolutions = %d, want 1", resolved)
	}
	found := false
	for _, n := range names {
		if strings.HasSuffix(n, "-issue-8-integration") {
			found = true
		}
	}
	if !found {
		t.Fatalf("integration worker not started, names = %v", names)
	}
	got, err := os.ReadFile(filepath.Join(intDir, "base.txt"))
	if err != nil || string(got) != "resolved\n" {
		t.Fatalf("base.txt = %q err=%v, want resolved", got, err)
	}
	if _, err := os.Stat(filepath.Join(intDir, "task9.txt")); err != nil {
		t.Fatalf("task file not merged: %v", err)
	}
}

// TestSchedulerConflictResolvedByAgent 两个无前置任务改同一文件：
// 先完成的直接合入，后完成的冲突 → 调度器发冲突提示词 →
// agent 在任务 worktree 解决并输出标记 → 快速合并成功。
func TestSchedulerConflictResolvedByAgent(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 10}, Title: "task B", Kind: graph.KindChild})

	sessions := &conflictSessions{fakeSessions: &fakeSessions{}, intBranch: intBranch}
	cfg := config.Config{Parallelism: 2, SpecIID: 8, Retries: 4, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, &fakeForge{}, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !g.Done() {
		t.Fatal("graph not done")
	}
	sessions.mu.Lock()
	conflicts := sessions.conflicts
	sessions.mu.Unlock()
	if conflicts != 1 {
		t.Fatalf("conflict prompts = %d, want 1", conflicts)
	}
	got, err := os.ReadFile(filepath.Join(intDir, "conflict.txt"))
	if err != nil || string(got) != "resolved\n" {
		t.Fatalf("conflict.txt = %q err=%v, want resolved", got, err)
	}
}

// TestSchedulerInFlightSkipped 看板 status 为进行中的 issue（他人或
// 别的 agent 正在处理）不领取：不 claim、不建 worktree、不启动
// worker，任务转 needs attention 后调度停滞报错。
func TestSchedulerInFlightSkipped(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})

	forge := &fakeForge{inFlight: map[int]bool{9: true}}
	sessions := &fakeSessions{}
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, forge, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	err = s.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "needs attention") {
		t.Fatalf("Run err = %v, want stalled needs-attention error", err)
	}
	forge.mu.Lock()
	claimed := append([]graph.Key{}, forge.claimed...)
	notes := append([]string{}, forge.notes...)
	forge.mu.Unlock()
	if len(claimed) != 0 {
		t.Fatalf("claimed %v, want none (in-flight issue must not be claimed)", claimed)
	}
	sessions.mu.Lock()
	started := len(sessions.started)
	sessions.mu.Unlock()
	if started != 0 {
		t.Fatalf("workers started = %d, want 0", started)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "处理中") {
		t.Fatalf("notes %v, want one in-flight notice", notes)
	}
}

// TestSchedulerRefreshInFlight 周期刷新同步占用状态：被他人接管的
// pending 节点标记 InFlight 后不再就绪；看板退回空闲后恢复就绪。
func TestSchedulerRefreshInFlight(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})

	forge := &fakeForge{inFlight: map[int]bool{9: true}}
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, forge, "gitlab", g, &fakeSessions{}, "tpl", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())

	s.refreshStates(ctx)
	n, _ := g.Lookup(graph.Key{ProjectID: 1, IID: 9})
	if !n.InFlight {
		t.Fatal("node not marked in-flight after refresh")
	}
	if ready := g.Ready(); len(ready) != 0 {
		t.Fatalf("ready %v, want empty (occupied node must not be ready)", ready)
	}
	forge.mu.Lock()
	forge.inFlight[9] = false
	forge.mu.Unlock()
	s.refreshStates(ctx)
	n, _ = g.Lookup(graph.Key{ProjectID: 1, IID: 9})
	if n.InFlight {
		t.Fatal("node still in-flight after status reverted")
	}
	if ready := g.Ready(); len(ready) != 1 {
		t.Fatalf("ready %v, want node back after in-flight cleared", ready)
	}
}

// TestSchedulerWarnsDirtyBoard 已关闭但看板未终态的 issue：
// 刷新时只 warn 一次提醒人工修正，不自动改、不重复刷日志。
func TestSchedulerWarnsDirtyBoard(t *testing.T) {
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild, Closed: true})

	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	forgeFake := &fakeForge{closedOn: map[int]bool{9: true}, boardDirty: map[int]bool{9: true}}
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, forgeFake, "gitlab", g, &fakeSessions{}, "tpl", repoDir, intDir, intBranch, baseName, []string{"devin"}, log)

	s.refreshStates(ctx)
	s.refreshStates(ctx) // 第二次刷新不应重复 warn
	out := buf.String()
	if !strings.Contains(out, "board status not in a done column") {
		t.Fatalf("log %q, want dirty-board warn", out)
	}
	if n := strings.Count(out, "board status not in a done column"); n != 1 {
		t.Fatalf("warn count = %d, want 1 (dedup)", n)
	}
}
