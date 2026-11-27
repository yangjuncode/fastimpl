package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nsgo-dev/nsgo/nsgotest"
	"github.com/yangjuncode/fastimpl/app"
	"github.com/yangjuncode/fastimpl/config"
)

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func initTestRepoWithWorktree(t *testing.T, repoDir, treeParent, wtName, branch string) {
	t.Helper()
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoDir, "init")
	runGitTest(t, repoDir, "config", "user.email", "test@example.com")
	runGitTest(t, repoDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoDir, "add", "README.md")
	runGitTest(t, repoDir, "commit", "-m", "init")
	if treeParent != "" {
		if err := os.MkdirAll(treeParent, 0o755); err != nil {
			t.Fatal(err)
		}
		runGitTest(t, repoDir, "worktree", "add", "-b", branch, filepath.Join(treeParent, wtName))
	}
}

func waitDiscoverSeq(t *testing.T, gui *nsgotest.App, m *model, minSeq uint64) snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gui.Settle()
		if snap := m.session.Snapshot(); snap.DiscoverSeq >= minSeq {
			gui.Settle()
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for DiscoverSeq >= %d; last snapshot = %+v", minSeq, m.session.Snapshot())
	return snapshot{}
}

// Discover 必须支持被后续选择抢占：后发起的发现取消前一次并覆盖快照，
// 前一次迟到的返回不得把新的结果与候选列表覆盖回去。
func TestDiscoverPropagatesCandidatesAndSupersedesInFlight(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstCanceled := make(chan struct{})

	s, _ := discoverSession(func(ctx context.Context, opts app.Options) (app.PathsPreview, error) {
		if opts.RepoDir == "" {
			close(firstStarted)
			select {
			case <-ctx.Done():
				close(firstCanceled)
			case <-time.After(time.Second):
			}
			<-releaseFirst
			return app.PathsPreview{
				Paths:          app.Paths{WorkDir: "/w"},
				RepoErr:        errors.New("stale multi repo"),
				RepoCandidates: []string{"/w/stale-a", "/w/stale-b"},
			}, nil
		}
		return app.PathsPreview{
			Paths:          app.Paths{WorkDir: "/w", RepoDir: opts.RepoDir, TreeDir: "/w/trees"},
			TreeCandidates: []string{"/w/trees-1", "/w/trees-2"},
		}, nil
	})

	seq1 := s.Discover(app.Options{WorkDir: "/w"})
	<-firstStarted
	seq2 := s.Discover(app.Options{WorkDir: "/w", RepoDir: "/w/repo-b"})
	if seq2 <= seq1 {
		t.Fatalf("seq2=%d must be > seq1=%d", seq2, seq1)
	}
	select {
	case <-firstCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("superseded Discover context was not canceled")
	}
	snap := waitCondition(t, s, "second discover result", func(snap snapshot) bool {
		return snap.DiscoverSeq == seq2
	})
	close(releaseFirst)
	time.Sleep(20 * time.Millisecond)

	snap = s.Snapshot()
	if snap.DiscoverSeq != seq2 || snap.Paths.RepoDir != "/w/repo-b" || snap.Paths.TreeDir != "/w/trees" {
		t.Fatalf("stale Discover overwrote newer snapshot: %+v", snap)
	}
	if len(snap.TreeCandidates) != 2 || snap.TreeCandidates[0] != "/w/trees-1" {
		t.Fatalf("TreeCandidates = %v, want propagated candidates", snap.TreeCandidates)
	}
}

// 端到端测试：
//  1. 工作目录与仓库目录不一致且检测到多个仓库时，启动表单列出全部候选仓库供选择；
//  2. 点击某个候选仓库后自动重新查找该仓库的 Work Tree 目录并填入；切换另一个候选仓库时
//     忽略旧仓库的 tree_dir 并重新查找新仓库的 Work Tree 目录；
//  3. 选择完成后自动将 repo_dir 与 tree_dir 保存到 <work-dir>/fastimpl.toml，下次在
//     同一工作目录重新打开即可直接定位到已保存的仓库与 Work Tree 目录。
func TestEmptyStateListsDetectedReposAndReinfersTreeAndPersists(t *testing.T) {
	workDir := t.TempDir()
	repoA := filepath.Join(workDir, "repo-alpha")
	repoB := filepath.Join(workDir, "repo-beta")
	treeA := filepath.Join(t.TempDir(), "wt-alpha")
	treeB := filepath.Join(t.TempDir(), "wt-beta")
	initTestRepoWithWorktree(t, repoA, treeA, "issue-1", "issue-1")
	initTestRepoWithWorktree(t, repoB, treeB, "issue-2", "issue-2")

	gui, m, _ := headlessWith(t, app.Options{WorkDir: workDir}, nil)
	snap := waitDiscoverSeq(t, gui, m, 1)

	if len(snap.RepoCandidates) != 2 {
		t.Fatalf("RepoCandidates = %v, want 2 detected repos", snap.RepoCandidates)
	}
	gui.Find("id:repo-candidate-list")
	gui.Find("id:repo-candidate-0 >> text:repo-alpha")
	gui.Find("id:repo-candidate-1 >> text:repo-beta")

	// 点击第一个仓库：应自动查找它的 Work Tree 目录，并落盘到 <workDir>/fastimpl.toml。
	gui.Press("id:repo-candidate-0")
	snap = waitDiscoverSeq(t, gui, m, 2)
	if !snap.PathsReady {
		t.Fatalf("after clicking repo-alpha: PathsReady=false, repoErr=%v, treeErr=%v", snap.RepoPathErr, snap.TreePathErr)
	}
	if !sameDir(m.repoText.GetUntracked(), repoA) {
		t.Fatalf("repoText = %q, want %q", m.repoText.GetUntracked(), repoA)
	}
	if !sameDir(m.treeText.GetUntracked(), treeA) {
		t.Fatalf("treeText = %q, want re-discovered %q", m.treeText.GetUntracked(), treeA)
	}
	gui.Find("id:form-save-hint")

	cfg, err := config.Load(workDir, "")
	if err != nil {
		t.Fatalf("config.Load after repo-alpha selection: %v", err)
	}
	if !sameDir(cfg.RepoDir, repoA) || !sameDir(cfg.TreeDir, treeA) {
		t.Fatalf("saved config = (repo=%q, tree=%q), want (%q, %q)", cfg.RepoDir, cfg.TreeDir, repoA, treeA)
	}

	// 改选第二个仓库：候选列表仍保持可见，Work Tree 必须重新查找成 treeB，且配置更新为 repoB + treeB。
	gui.Press("id:repo-candidate-1")
	snap = waitDiscoverSeq(t, gui, m, 3)
	if !snap.PathsReady {
		t.Fatalf("after switching to repo-beta: PathsReady=false, repoErr=%v, treeErr=%v", snap.RepoPathErr, snap.TreePathErr)
	}
	if !sameDir(m.repoText.GetUntracked(), repoB) {
		t.Fatalf("repoText = %q, want %q", m.repoText.GetUntracked(), repoB)
	}
	if !sameDir(m.treeText.GetUntracked(), treeB) {
		t.Fatalf("treeText = %q, want re-discovered %q (must not retain stale treeA)", m.treeText.GetUntracked(), treeB)
	}

	// 模拟下次在同一个工作目录重新打开 fastimpl：不传任何 -repo-dir/-tree-dir 参数，
	// 应直接命中保存的配置，无需重新选择仓库或 Work Tree。
	gui2, m2, _ := headlessWith(t, app.Options{WorkDir: workDir}, nil)
	snap2 := waitDiscoverSeq(t, gui2, m2, 1)
	if !snap2.PathsReady {
		t.Fatalf("reopen in workDir: PathsReady=false, repoErr=%v, treeErr=%v", snap2.RepoPathErr, snap2.TreePathErr)
	}
	if !sameDir(m2.repoText.GetUntracked(), repoB) || !sameDir(m2.treeText.GetUntracked(), treeB) {
		t.Fatalf("reopen paths = (repo=%q, tree=%q), want (%q, %q)", m2.repoText.GetUntracked(), m2.treeText.GetUntracked(), repoB, treeB)
	}
	if gui2.Exists("id:repo-candidate-list") {
		t.Fatal("reopen should resolve saved repo directly without showing repo-candidate-list")
	}
}

// 多个 Work Tree 父目录候选时，启动表单应列出候选项，点选后自动保存且下次打开可复用。
func TestEmptyStateListsTreeCandidatesAndPersistsSelection(t *testing.T) {
	workDir := t.TempDir()
	repoDir := filepath.Join(workDir, "single-repo")
	tree1 := filepath.Join(t.TempDir(), "trees-1")
	tree2 := filepath.Join(t.TempDir(), "trees-2")
	initTestRepoWithWorktree(t, repoDir, tree1, "issue-1", "issue-1")
	if err := os.MkdirAll(tree2, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoDir, "worktree", "add", "-b", "issue-2", filepath.Join(tree2, "issue-2"))

	gui, m, _ := headlessWith(t, app.Options{WorkDir: workDir}, nil)
	snap := waitDiscoverSeq(t, gui, m, 1)

	if len(snap.TreeCandidates) != 2 {
		t.Fatalf("TreeCandidates = %v, want 2 tree parent candidates", snap.TreeCandidates)
	}
	gui.Find("id:tree-candidate-list")

	picked := snap.TreeCandidates[1]
	gui.Press("id:tree-candidate-1")
	snap = waitDiscoverSeq(t, gui, m, 2)
	if !snap.PathsReady {
		t.Fatalf("after picking tree candidate: PathsReady=false, err=%v", snap.TreePathErr)
	}
	if !sameDir(m.treeText.GetUntracked(), picked) {
		t.Fatalf("treeText = %q, want %q", m.treeText.GetUntracked(), picked)
	}

	reopen, err := app.PreviewPaths(context.Background(), app.Options{WorkDir: workDir})
	if err != nil || !reopen.Ready() {
		t.Fatalf("reopen PreviewPaths = (%+v, %v), want ready", reopen, err)
	}
	if !sameDir(reopen.Paths.TreeDir, picked) {
		t.Fatalf("reopen TreeDir = %q, want %q", reopen.Paths.TreeDir, picked)
	}
}

// 手动输入外部仓库路径并提交（回车/失焦）时，也必须重新查找 Work Tree 并保存到工作目录配置。
func TestManualRepoInputCommitReinfersTreeAndPersists(t *testing.T) {
	workDir := t.TempDir()
	extRepo := filepath.Join(t.TempDir(), "external-repo")
	extTree := filepath.Join(t.TempDir(), "external-trees")
	initTestRepoWithWorktree(t, extRepo, extTree, "issue-9", "issue-9")

	gui, m, _ := headlessWith(t, app.Options{WorkDir: workDir}, nil)
	waitDiscoverSeq(t, gui, m, 1)

	gui.Post(func() {
		m.repoText.Set(extRepo)
		m.commitRepoInput()
	})
	snap := waitDiscoverSeq(t, gui, m, 2)
	if !snap.PathsReady {
		t.Fatalf("after committing manual repo: PathsReady=false, repoErr=%v, treeErr=%v", snap.RepoPathErr, snap.TreePathErr)
	}
	if !sameDir(m.treeText.GetUntracked(), extTree) {
		t.Fatalf("treeText = %q, want re-discovered %q", m.treeText.GetUntracked(), extTree)
	}

	cfgPath := filepath.Join(workDir, config.DefaultFile)
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("expected %s to be saved when workDir != repoDir: %v", cfgPath, err)
	}
}

// 工作目录与仓库目录相同时，不应无故在仓库根目录自动生成 fastimpl.toml。
func TestSameWorkAndRepoDirDoesNotAutoWriteConfig(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "same-dir-repo")
	treeDir := filepath.Join(t.TempDir(), "same-dir-trees")
	initTestRepoWithWorktree(t, repoDir, treeDir, "issue-1", "issue-1")

	gui, m, _ := headlessWith(t, app.Options{WorkDir: repoDir}, nil)
	snap := waitDiscoverSeq(t, gui, m, 1)
	if !snap.PathsReady {
		t.Fatalf("same-dir discover not ready: repoErr=%v, treeErr=%v", snap.RepoPathErr, snap.TreePathErr)
	}
	if _, err := os.Stat(filepath.Join(repoDir, config.DefaultFile)); !os.IsNotExist(err) {
		t.Fatalf("unexpected fastimpl.toml created when workDir == repoDir: err=%v", err)
	}
}
