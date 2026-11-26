package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yangjuncode/fastimpl/graph"
)

// recoveryCandidate 读取并验证同 issue 的 active metadata。返回 nil,nil
// 表示没有可恢复记录；返回 error 表示不能安全接管也不能当作普通新任务
// 启动（活跃 supervisor、卡住的 supervisor、schema/身份不匹配、终端死亡、
// integrating 等），调用方应转 needs attention 而不是偷跑。
func (s *Scheduler) recoveryCandidate(ctx context.Context, key graph.Key) (*ActiveTaskMeta, error) {
	path := ActiveMetaPath(s.repo.Root, key.IID)
	meta, err := readActiveMeta(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read active worker metadata %s: %w", path, err)
	}
	switch state := meta.OwnerState(time.Now()); state {
	case OwnerActive:
		return nil, fmt.Errorf("issue is actively supervised by fastimpl instance %s pid %d (heartbeat %s); observe only while it is alive",
			meta.Supervisor.InstanceID, meta.Supervisor.PID, meta.Supervisor.HeartbeatAt.Format(time.RFC3339))
	case OwnerBlocked:
		return nil, fmt.Errorf("fastimpl supervisor pid %d is alive but its heartbeat is stale (%s); refusing to risk dual control",
			meta.Supervisor.PID, meta.Supervisor.HeartbeatAt.Format(time.RFC3339))
	}
	if err := s.validateRecovery(ctx, key, meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// validateRecovery 做全部只读安全核验。只有通过后才允许偷本机锁；
// 远端 claim、issue 指派、hc 终端身份、worktree、agent 命令与集成分支
// 都必须仍指向同一份工作。
func (s *Scheduler) validateRecovery(ctx context.Context, key graph.Key, meta ActiveTaskMeta) error {
	if meta.ProjectID != key.ProjectID || meta.IssueIID != key.IID || meta.SpecIID != s.specIID {
		return fmt.Errorf("active metadata identity mismatch: have %d!%d spec %d, want %v spec %d",
			meta.ProjectID, meta.IssueIID, meta.SpecIID, key, s.specIID)
	}
	if CanonicalRepoPath(meta.RepoDir) != CanonicalRepoPath(s.repo.Root) {
		return fmt.Errorf("active metadata repo %s does not match %s", meta.RepoDir, s.repo.Root)
	}
	if meta.IntegrationBranch != s.branch || meta.IntegrationDir != s.intDir {
		return fmt.Errorf("active metadata integration target changed (%s %s vs %s %s); refusing automatic recovery",
			meta.IntegrationBranch, meta.IntegrationDir, s.branch, s.intDir)
	}
	switch meta.Stage {
	case activeStageBooting, activeStageWaitingReport, activeStageIntegrating:
	default:
		return fmt.Errorf("active worker is at stage %q; automatic recovery is only safe while booting, waiting for a report, or re-entering an idempotent integration step", meta.Stage)
	}
	if meta.ClaimToken == "" || meta.ReportToken == "" || meta.TerminalAddr == "" || meta.TerminalName == "" {
		return errors.New("active metadata is missing claim/report tokens or hc terminal identity")
	}
	if meta.Worktree == "" || meta.Branch == "" {
		return errors.New("active metadata is missing worktree or branch")
	}
	if st, err := os.Stat(meta.Worktree); err != nil || !st.IsDir() {
		return fmt.Errorf("recovered worktree %s is unavailable: %v", meta.Worktree, err)
	}
	if err := s.repo.VerifyWorktree(ctx, meta.Worktree, meta.Branch); err != nil {
		return fmt.Errorf("recovered worktree identity mismatch: %w", err)
	}
	if err := s.repo.VerifyWorktree(ctx, meta.IntegrationDir, meta.IntegrationBranch); err != nil {
		return fmt.Errorf("recovered integration worktree identity mismatch: %w", err)
	}
	if meta.TerminalCwd == "" || meta.TerminalPID <= 0 || len(meta.TerminalArgv) == 0 {
		return errors.New("active metadata is missing the terminal cwd/pid/argv process identity")
	}
	if agentArgvHash(meta.TerminalArgv) != agentArgvHash(s.agentArgv) {
		return fmt.Errorf("active terminal agent command changed; inspect hc terminal %s before reuse", meta.TerminalName)
	}
	record, err := s.sessions.Snapshot(ctx, meta.TerminalAddr)
	if err != nil {
		return fmt.Errorf("read recovered hc terminal %s: %w", meta.TerminalAddr, err)
	}
	if !record.Live {
		return fmt.Errorf("recovered hc terminal %s is not live; refusing to restart over possible completed output", meta.TerminalAddr)
	}
	if record.Name != meta.TerminalName || record.Labels["fastimpl"] != fmt.Sprint(s.specIID) || record.Labels["task"] != meta.TerminalName {
		return fmt.Errorf("hc terminal %s identity changed; refusing to reuse or replace it", meta.TerminalName)
	}
	if record.PID <= 0 || record.Cwd == "" || len(record.Argv) == 0 {
		return fmt.Errorf("hc terminal %s does not expose verifiable cwd/pid/argv; refusing automatic recovery", meta.TerminalName)
	}
	if record.PID != meta.TerminalPID || CanonicalRepoPath(record.Cwd) != CanonicalRepoPath(meta.TerminalCwd) ||
		agentArgvHash(record.Argv) != agentArgvHash(meta.TerminalArgv) {
		return fmt.Errorf("hc terminal %s process identity changed; refusing to reuse or replace it", meta.TerminalName)
	}
	if meta.IntegrationPhase == integrationPhaseResolvingBaseConflict {
		if meta.IntegrationTerminalAddr == "" || meta.IntegrationTerminalName == "" || meta.IntegrationToken == "" || !meta.IntegrationPromptSent {
			return errors.New("base-conflict integration metadata is incomplete; refusing to risk a duplicate integration agent")
		}
		if meta.IntegrationTerminalCwd == "" || meta.IntegrationTerminalPID <= 0 || len(meta.IntegrationTerminalArgv) == 0 {
			return errors.New("base-conflict integration metadata is missing the terminal cwd/pid/argv process identity")
		}
		if agentArgvHash(meta.IntegrationTerminalArgv) != agentArgvHash(s.agentArgv) {
			return fmt.Errorf("integration terminal agent command changed; inspect hc terminal %s before reuse", meta.IntegrationTerminalName)
		}
		intRecord, err := s.sessions.Snapshot(ctx, meta.IntegrationTerminalAddr)
		if err != nil {
			return fmt.Errorf("read recovered integration terminal %s: %w", meta.IntegrationTerminalAddr, err)
		}
		if !intRecord.Live || intRecord.Name != meta.IntegrationTerminalName ||
			intRecord.Labels["fastimpl"] != fmt.Sprint(s.specIID) || intRecord.Labels["task"] != meta.IntegrationTerminalName {
			return fmt.Errorf("integration terminal %s identity changed; refusing automatic base-conflict recovery", meta.IntegrationTerminalName)
		}
		if intRecord.PID <= 0 || intRecord.Cwd == "" || len(intRecord.Argv) == 0 {
			return fmt.Errorf("integration terminal %s does not expose verifiable cwd/pid/argv; refusing automatic recovery", meta.IntegrationTerminalName)
		}
		if intRecord.PID != meta.IntegrationTerminalPID || CanonicalRepoPath(intRecord.Cwd) != CanonicalRepoPath(meta.IntegrationTerminalCwd) ||
			agentArgvHash(intRecord.Argv) != agentArgvHash(meta.IntegrationTerminalArgv) {
			return fmt.Errorf("integration terminal %s process identity changed; refusing to reuse or replace it", meta.IntegrationTerminalName)
		}
	}
	info, err := s.forge.IssueState(ctx, key.ProjectID, key.IID)
	if err != nil {
		return fmt.Errorf("read issue state for recovery: %w", err)
	}
	if info.Open && (!info.AssignedToMe || info.AssignedToOther) {
		return fmt.Errorf("issue assignment changed while the supervisor was gone (me=%v other=%v)", info.AssignedToMe, info.AssignedToOther)
	}
	winner, mine, err := s.boundedRead(ctx, key, meta.ClaimToken)
	if err != nil {
		return fmt.Errorf("verify recovered claim: %w", err)
	}
	if !holdsClaim(winner, mine) {
		return errors.New("the original cross-machine claim is no longer active and earliest; refusing automatic recovery")
	}
	return nil
}

func noopAdoptionRelease() error { return nil }

func lockClaimToken(content string) string {
	for line := range strings.Lines(content) {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == "claim" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// acquireTaskLockAfterRecovery 偷回已死 supervisor 的本机锁。接管临界区由
// flock 保护：两个新实例可能同时看到“旧 supervisor 已死”，但只有一个能
// 删除旧锁、创建新锁并把 metadata 改写成自己的 supervisor。flock 随进程
// 死亡自动释放，不会像普通锁文件一样卡在接管路径上。
// 返回的 releaseAdoption 必须在新 metadata 落盘后调用；它只保护接管启动段，
// 不会在任务运行期间长期占用。
func acquireTaskLockAfterRecovery(repoDir string, iid int, content string, recovered bool, expected *ActiveTaskMeta) (*taskLock, func() error, error) {
	if !recovered {
		lock, err := AcquireTaskLock(repoDir, iid, content)
		return lock, noopAdoptionRelease, err
	}
	if expected == nil {
		return nil, noopAdoptionRelease, fmt.Errorf("active recovery requires the expected metadata")
	}
	release, err := lockRecoveryAdoption(TaskLockPath(repoDir, iid) + ".adopt")
	if err != nil {
		return nil, noopAdoptionRelease, err
	}
	fail := func(err error) (*taskLock, func() error, error) {
		_ = release()
		return nil, noopAdoptionRelease, err
	}
	// 进入 flock 临界区后必须重读 metadata：等待 mutex 期间另一个实例可能
	// 已完成接管并把 supervisor 改成自己。只有仍指向同一份已死亡现场时，
	// 才允许删除它留下的 lock。
	current, err := readActiveMeta(ActiveMetaPath(repoDir, iid))
	if err != nil {
		return fail(fmt.Errorf("re-read active worker metadata inside the adoption lock: %w", err))
	}
	if current.ClaimToken != expected.ClaimToken || current.ReportToken != expected.ReportToken ||
		current.Supervisor.PID != expected.Supervisor.PID || current.Supervisor.StartID != expected.Supervisor.StartID ||
		current.Supervisor.InstanceID != expected.Supervisor.InstanceID {
		return fail(fmt.Errorf("active worker metadata changed while waiting for the adoption lock; refusing to steal a newer owner's lock"))
	}
	if state := current.OwnerState(time.Now()); state != OwnerAdoptable {
		return fail(fmt.Errorf("active worker owner is %s inside the adoption lock; refusing to steal its lock", state))
	}
	path := TaskLockPath(repoDir, iid)
	// flock 只串行化协作的 fastimpl 接管者，不能证明路径仍属于旧 supervisor。
	// 删除前核对它仍是普通文件且 claim token 与 metadata 一致；否则把现场
	// 留给人工，而不是误删被替换/手工放置的 lock。
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return fail(fmt.Errorf("dead supervisor lock %s is not a regular file; refusing to remove it", path))
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fail(fmt.Errorf("read dead supervisor lock %s: %w", path, readErr))
		}
		if lockClaimToken(string(data)) != expected.ClaimToken {
			return fail(fmt.Errorf("dead supervisor lock %s claim does not match active metadata; refusing to remove it", path))
		}
	} else if !os.IsNotExist(statErr) {
		return fail(fmt.Errorf("stat dead supervisor lock %s: %w", path, statErr))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fail(fmt.Errorf("remove dead supervisor lock %s: %w", path, err))
	}
	lock, err := AcquireTaskLock(repoDir, iid, content)
	if err != nil {
		return fail(err)
	}
	return lock, release, nil
}
