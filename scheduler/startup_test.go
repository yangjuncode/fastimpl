package scheduler

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
	"github.com/yangjuncode/fastimpl/worktree"
)

// startupTestRig 组装一个单任务 scheduler，worker 会在自己的 worktree
// 里提交文件，因此任务可以完整走完集成路径。
func startupTestRig(t *testing.T, f forge.Forge, sessions Sessions) (*Scheduler, context.Context) {
	t.Helper()
	taskLockDir(t)
	repoDir := initRepo(t)
	repo := worktree.NewRepo(repoDir, filepath.Join(repoDir, ".wt"))
	ctx := context.Background()
	intBranch, intDir, baseName, err := repo.EnsureIntegration(ctx, 8, "", "")
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New(graph.Key{ProjectID: 1, IID: 8})
	g.Add(graph.Node{Key: graph.Key{ProjectID: 1, IID: 9}, Title: "task A", Kind: graph.KindChild})
	cfg := config.Config{Parallelism: 1, SpecIID: 8, InstanceID: testInstanceID}
	s := newTestScheduler(cfg, repo, f, "gitlab", g, sessions, "{{.IssueNo}}|{{.Token}}|{{.SpecNo}}", repoDir, intDir, intBranch, baseName, []string{"devin"}, slog.Default())
	return s, ctx
}

func waitClosedTest(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// firstReadGateForge 只拦住第一次 IssueState/IssueComments。若两者串行，
// 第二个 entered 永远不会关闭；并行实现会先同时到达这里。
type firstReadGateForge struct {
	*fakeForge
	stateCalls   atomic.Int32
	commentCalls atomic.Int32
	stateEntered chan struct{}
	commentsSeen chan struct{}
	release      chan struct{}
}

func (f *firstReadGateForge) IssueState(ctx context.Context, projectID, iid int) (forge.IssueInfo, error) {
	if f.stateCalls.Add(1) == 1 {
		close(f.stateEntered)
		select {
		case <-f.release:
		case <-ctx.Done():
			return forge.IssueInfo{}, ctx.Err()
		}
	}
	return f.fakeForge.IssueState(ctx, projectID, iid)
}

func (f *firstReadGateForge) IssueComments(ctx context.Context, projectID, iid int) ([]forge.IssueComment, error) {
	if f.commentCalls.Add(1) == 1 {
		close(f.commentsSeen)
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fakeForge.IssueComments(ctx, projectID, iid)
}

func TestStartupReadsIssueStateAndCommentsInParallel(t *testing.T) {
	base := &fakeForge{}
	f := &firstReadGateForge{
		fakeForge:    base,
		stateEntered: make(chan struct{}),
		commentsSeen: make(chan struct{}),
		release:      make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.release) }) }
	defer release()

	sessions := &commitSessions{fakeSessions: &fakeSessions{}, t: t}
	s, ctx := startupTestRig(t, f, sessions)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	waitClosedTest(t, "the first issue state read", f.stateEntered)
	waitClosedTest(t, "the first comment read before releasing issue state", f.commentsSeen)
	var rt TaskRuntime
	for _, item := range s.Status() {
		if item.Key == (graph.Key{ProjectID: 1, IID: 9}) && !item.Integration {
			rt = item
		}
	}
	if !rt.Starting || rt.Addr != "" {
		t.Fatalf("runtime during startup reads = %#v, want starting without an address", rt)
	}
	release()
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// secondCommentGateForge 拦住 claim 后的第一次仲裁复查。预热实现会在这个
// 复查尚未完成时就调用 StartWorker；旧串行实现不会。
type secondCommentGateForge struct {
	*fakeForge
	commentCalls atomic.Int32
	secondSeen   chan struct{}
	release      chan struct{}
}

func (f *secondCommentGateForge) IssueComments(ctx context.Context, projectID, iid int) ([]forge.IssueComment, error) {
	if f.commentCalls.Add(1) == 2 {
		close(f.secondSeen)
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fakeForge.IssueComments(ctx, projectID, iid)
}

type notifyStartSessions struct {
	Sessions
	once    sync.Once
	started chan struct{}
}

func (n *notifyStartSessions) StartWorker(ctx context.Context, p worker.Params) (string, error) {
	addr, err := n.Sessions.StartWorker(ctx, p)
	if err == nil {
		n.once.Do(func() { close(n.started) })
	}
	return addr, err
}

func TestWarmWorkerStartsBeforeClaimValidationFinishes(t *testing.T) {
	base := &fakeForge{}
	f := &secondCommentGateForge{
		fakeForge:  base,
		secondSeen: make(chan struct{}),
		release:    make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.release) }) }
	defer release()

	commit := &commitSessions{fakeSessions: &fakeSessions{}, t: t}
	sessions := &notifyStartSessions{Sessions: commit, started: make(chan struct{})}
	s, ctx := startupTestRig(t, f, sessions)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	waitClosedTest(t, "the blocked post-claim arbitration read", f.secondSeen)
	waitClosedTest(t, "warm worker startup before claim validation finishes", sessions.started)
	release()
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// firstClaimNoteGateForge 只拦住第一条 claim 评论。预热实现会在这次
// forge POST 尚未返回时就调用 StartWorker；串行实现不会。
type firstClaimNoteGateForge struct {
	*fakeForge
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (f *firstClaimNoteGateForge) Note(ctx context.Context, projectID, iid int, message string) error {
	if strings.HasPrefix(message, claimVersion+" claim ") {
		f.once.Do(func() { close(f.entered) })
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.fakeForge.Note(ctx, projectID, iid, message)
}

func TestWarmWorkerStartsWhileClaimPostInFlight(t *testing.T) {
	base := &fakeForge{}
	f := &firstClaimNoteGateForge{
		fakeForge: base,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.release) }) }
	defer release()

	commit := &commitSessions{fakeSessions: &fakeSessions{}, t: t}
	sessions := &notifyStartSessions{Sessions: commit, started: make(chan struct{})}
	s, ctx := startupTestRig(t, f, sessions)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	waitClosedTest(t, "the blocked claim note post", f.entered)
	waitClosedTest(t, "warm worker startup while claim post is still in flight", sessions.started)
	var rt TaskRuntime
	for _, item := range s.Status() {
		if item.Key == (graph.Key{ProjectID: 1, IID: 9}) && !item.Integration {
			rt = item
		}
	}
	if rt.Addr == "" {
		t.Fatalf("warm worker address is empty while StartWorker has completed: %#v", rt)
	}
	if !rt.Starting || rt.StartupPhase == "" {
		t.Fatalf("runtime after warm address publication = %#v, want a visible startup phase until prompt", rt)
	}
	release()
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
	rt = TaskRuntime{}
	for _, item := range s.Status() {
		if item.Key == (graph.Key{ProjectID: 1, IID: 9}) && !item.Integration {
			rt = item
		}
	}
	if rt.Starting || rt.StartupPhase != "" {
		t.Fatalf("runtime after prompt completion = %#v, want startup progress cleared", rt)
	}
}

// blockingMarkForge 拦住 MarkInProgress。它已后台化，因此 prompt 不应等
// 这次看板 mutation 完成。
type blockingMarkForge struct {
	*fakeForge
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (f *blockingMarkForge) MarkInProgress(ctx context.Context, projectID, iid int) error {
	f.once.Do(func() { close(f.entered) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return f.fakeForge.MarkInProgress(ctx, projectID, iid)
}

type notifyPromptSessions struct {
	Sessions
	once   sync.Once
	prompt chan struct{}
}

func (n *notifyPromptSessions) Prompt(ctx context.Context, addr, text string) (uint64, error) {
	n.once.Do(func() { close(n.prompt) })
	return n.Sessions.Prompt(ctx, addr, text)
}

func TestPromptDoesNotWaitForMarkInProgress(t *testing.T) {
	base := &fakeForge{}
	f := &blockingMarkForge{
		fakeForge: base,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.release) }) }
	defer release()

	commit := &commitSessions{fakeSessions: &fakeSessions{}, t: t}
	sessions := &notifyPromptSessions{Sessions: commit, prompt: make(chan struct{})}
	s, ctx := startupTestRig(t, f, sessions)
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	waitClosedTest(t, "the blocked in-progress board mutation", f.entered)
	waitClosedTest(t, "prompt while MarkInProgress is still blocked", sessions.prompt)
	release()
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestPromptDoesNotWaitForIntegrationPrep 初始 push/EnsureMR 也不应挡住首个
// prompt；但 run 不能在准备仍blocked时完成，因为真正合并必须先等它收束。
func TestPromptDoesNotWaitForIntegrationPrep(t *testing.T) {
	f := &fakeForge{}
	commit := &commitSessions{fakeSessions: &fakeSessions{}, t: t}
	sessions := &notifyPromptSessions{Sessions: commit, prompt: make(chan struct{})}
	s, ctx := startupTestRig(t, f, sessions)

	prepEntered := make(chan struct{})
	prepRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(prepRelease) }) }
	defer release()
	s.PrepareIntegrationAsync(ctx, func(pctx context.Context) (string, error) {
		close(prepEntered)
		select {
		case <-prepRelease:
			return "http://mr/1", nil
		case <-pctx.Done():
			return "", pctx.Err()
		}
	})

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	waitClosedTest(t, "the blocked integration preparation", prepEntered)
	waitClosedTest(t, "prompt while integration preparation is still blocked", sessions.prompt)
	select {
	case err := <-runErr:
		t.Fatalf("Run finished before blocked integration preparation completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
