package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yangjuncode/fastimpl/config"
	"github.com/yangjuncode/fastimpl/graph"
	"github.com/yangjuncode/fastimpl/worker"
)

// terminalID 区分普通任务终端与集成专用终端（二者都可能使用 spec key）。
type terminalID struct {
	key         graph.Key
	integration bool
}

type retainedTerminal struct {
	addr     string
	label    string
	metaPath string
	timer    *time.Timer
}

// retainedTerminalMeta 是跨进程/跨重启接管 retained terminal 所需的元数据。
// 它故意不放在 lock 文件中：lock 文件存在代表互斥锁，而 retained terminal
// 所属任务已经完成、不应再阻止同 issue 重跑。
type retainedTerminalMeta struct {
	Version      int       `json:"version"`
	RepoDir      string    `json:"repo_dir"`
	SpecIID      int       `json:"spec_iid"`
	IssueIID     int       `json:"issue_iid"`
	Integration  bool      `json:"integration"`
	TerminalAddr string    `json:"terminal_addr"`
	TerminalName string    `json:"terminal_name"`
	Cwd          string    `json:"cwd"`
	Branch       string    `json:"branch"`
	Commit       string    `json:"commit,omitempty"`
	AgentHash    string    `json:"agent_hash"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	CompletedAt  time.Time `json:"completed_at"`
	CleanupAt    time.Time `json:"cleanup_at"` // zero 表示 never
}

func repoHash(repoDir string) string {
	sum := sha256.Sum256([]byte(CanonicalRepoPath(repoDir)))
	return hex.EncodeToString(sum[:])
}

func retainedMetaPath(repoDir string, specIID, issueIID int, integration bool) string {
	name := fmt.Sprintf("fastimpl-%s-spec%d-issue%d", repoHash(repoDir), specIID, issueIID)
	if integration {
		name += "-integration"
	}
	return filepath.Join(os.TempDir(), name+".retained.json")
}

func retainedMetaPattern(repoDir string, specIID int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("fastimpl-%s-spec%d-issue*.retained.json", repoHash(repoDir), specIID))
}

func writeRetainedMeta(path string, meta retainedTerminalMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readRetainedMeta(path string) (retainedTerminalMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return retainedTerminalMeta{}, err
	}
	var meta retainedTerminalMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return retainedTerminalMeta{}, err
	}
	return meta, nil
}

func agentArgvHash(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (s *Scheduler) retentionPolicy() (time.Duration, bool) {
	delay, never, err := config.ParseTerminalRetention(s.cfg.TerminalRetention)
	if err != nil {
		return 0, false
	}
	return delay, never
}

func (s *Scheduler) retentionSignalLocked() {
	if s.retentionChanged != nil {
		close(s.retentionChanged)
	}
	s.retentionChanged = make(chan struct{})
}

func (s *Scheduler) initRetentionLocked() {
	if s.retained == nil {
		s.retained = make(map[terminalID]*retainedTerminal)
	}
	if s.retentionChanged == nil {
		s.retentionChanged = make(chan struct{})
	}
}

func (s *Scheduler) cancelRetained(id terminalID) {
	s.retentionMu.Lock()
	defer s.retentionMu.Unlock()
	s.initRetentionLocked()
	if rt := s.retained[id]; rt != nil && rt.timer != nil {
		rt.timer.Stop()
	}
	delete(s.retained, id)
	s.retentionSignalLocked()
}

// RetentionPending 报告是否还有 delayed terminal 的后台清理未完成。
func (s *Scheduler) RetentionPending() bool {
	s.retentionMu.Lock()
	defer s.retentionMu.Unlock()
	return len(s.retained) > 0
}

// WaitRetention 等全部 delayed terminal 清理尝试结束；只由 app 用来延后
// 关闭 hc 控制连接，不阻塞调度本身。
func (s *Scheduler) WaitRetention() {
	for {
		s.retentionMu.Lock()
		s.initRetentionLocked()
		if len(s.retained) == 0 {
			s.retentionMu.Unlock()
			return
		}
		ch := s.retentionChanged
		s.retentionMu.Unlock()
		<-ch
	}
}

func (s *Scheduler) scheduleRetainedCleanup(id terminalID, addr, label, metaPath string, delay time.Duration) {
	s.retentionMu.Lock()
	s.initRetentionLocked()
	if old := s.retained[id]; old != nil && old.timer != nil {
		old.timer.Stop()
	}
	rt := &retainedTerminal{addr: addr, label: label, metaPath: metaPath}
	rt.timer = time.AfterFunc(delay, func() { s.cleanupRetained(id) })
	s.retained[id] = rt
	s.retentionMu.Unlock()
}

func (s *Scheduler) cleanupRetained(id terminalID) {
	s.retentionMu.Lock()
	s.initRetentionLocked()
	rt := s.retained[id]
	s.retentionMu.Unlock()
	if rt == nil {
		return
	}
	// 清理期间仍保留 pending 记录：app 的 WaitRetention 必须等 Stop 调用
	// 本身结束，不能定时器一开始就把 hc 控制连接关掉。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := s.sessions.Stop(ctx, rt.addr, s.specIID, rt.label)
	cancel()
	s.retentionMu.Lock()
	if s.retained[id] == rt {
		delete(s.retained, id)
		s.retentionSignalLocked()
	}
	s.retentionMu.Unlock()
	if err != nil {
		s.log.Warn("retained terminal cleanup failed; leaving it for inspection/retry", "key", id.key, "addr", rt.addr, "err", err)
		return
	}
	s.runtime.clearAddr(id.key)
	if err := os.Remove(rt.metaPath); err != nil && !os.IsNotExist(err) {
		s.log.Warn("remove retained terminal metadata failed", "path", rt.metaPath, "err", err)
	}
	s.log.Info("retained terminal cleaned", "key", id.key, "addr", rt.addr)
}

// retainTerminal 在任务成功完成后按 terminal_retention 保留终端。
func (s *Scheduler) retainTerminal(key graph.Key, issueIID int, integration bool, addr, termName, cwd, branch, commit string) {
	delay, never := s.retentionPolicy()
	if delay == 0 && !never {
		return
	}
	completedAt := time.Now()
	// 成功路径在这里已经确定结局；立即清理路径则由主循环 record 补齐。
	// 先写 FinishedAt，metadata 才能完整恢复重启后的 tooltip。
	s.runtime.finish(key, completedAt)
	startedAt, finishedAt := s.runtime.times(key)
	if !finishedAt.IsZero() {
		completedAt = finishedAt
	}
	cleanupAt := time.Time{}
	if !never {
		cleanupAt = time.Now().Add(delay)
	}
	meta := retainedTerminalMeta{
		Version:      1,
		RepoDir:      CanonicalRepoPath(s.repo.Root),
		SpecIID:      s.specIID,
		IssueIID:     issueIID,
		Integration:  integration,
		TerminalAddr: addr,
		TerminalName: termName,
		Cwd:          CanonicalRepoPath(cwd),
		Branch:       branch,
		Commit:       commit,
		AgentHash:    agentArgvHash(s.agentArgv),
		StartedAt:    startedAt,
		CompletedAt:  completedAt,
		CleanupAt:    cleanupAt,
	}
	path := retainedMetaPath(s.repo.Root, s.specIID, issueIID, integration)
	if err := writeRetainedMeta(path, meta); err != nil {
		s.log.Warn("write retained terminal metadata failed; restart reconciliation unavailable", "path", path, "err", err)
	}
	if never {
		s.log.Info("worker terminal retained indefinitely", "key", key, "inspect", "hc attach "+termName)
		return
	}
	s.log.Info("worker terminal retained temporarily", "key", key, "delay", delay, "inspect", "hc attach "+termName)
	s.scheduleRetainedCleanup(terminalID{key: key, integration: integration}, addr, termName, path, delay)
}

func retainedRecordMatches(record worker.Record, meta retainedTerminalMeta) bool {
	return record.Name == meta.TerminalName &&
		record.Labels["fastimpl"] == strconv.Itoa(meta.SpecIID) &&
		record.Labels["task"] == meta.TerminalName
}

// reconcileRetained 接管上次进程留下的 retained terminal：过期立即清，
// 未过期重排定时器，never 只重新暴露给 GUI。
func (s *Scheduler) reconcileRetained(ctx context.Context) {
	matches, err := filepath.Glob(retainedMetaPattern(s.repo.Root, s.specIID))
	if err != nil || len(matches) == 0 {
		return
	}
	for _, path := range matches {
		meta, err := readRetainedMeta(path)
		if err != nil || meta.Version != 1 || meta.SpecIID != s.specIID {
			s.log.Warn("discard invalid retained terminal metadata", "path", path)
			_ = os.Remove(path)
			continue
		}
		key := graph.Key{ProjectID: s.g.Spec().ProjectID, IID: meta.IssueIID}
		record, err := s.sessions.Snapshot(ctx, meta.TerminalAddr)
		if err != nil || !record.Live {
			// 终端已经不在：保留目标已经达成，清掉 stale metadata。
			_ = os.Remove(path)
			continue
		}
		if !retainedRecordMatches(record, meta) {
			s.log.Warn("retained terminal identity mismatch; metadata discarded without touching terminal", "addr", meta.TerminalAddr, "name", meta.TerminalName)
			_ = os.Remove(path)
			continue
		}
		s.runtime.setAddr(key, meta.TerminalAddr, meta.TerminalName, meta.Integration)
		s.runtime.setTimes(key, meta.StartedAt, meta.CompletedAt)
		if meta.CleanupAt.IsZero() {
			continue
		}
		remaining := time.Until(meta.CleanupAt)
		s.scheduleRetainedCleanup(terminalID{key: key, integration: meta.Integration}, meta.TerminalAddr, meta.TerminalName, path, max(remaining, 0))
		if remaining <= 0 {
			s.log.Info("expired retained terminal scheduled for cleanup", "key", key, "addr", meta.TerminalAddr)
		}
	}
}

func (s *Scheduler) staticRetainedMatch(meta retainedTerminalMeta, issueIID int, integration bool, termName, cwd string) bool {
	return meta.Version == 1 &&
		meta.RepoDir == CanonicalRepoPath(s.repo.Root) &&
		meta.SpecIID == s.specIID &&
		meta.IssueIID == issueIID &&
		meta.Integration == integration &&
		meta.TerminalName == termName &&
		meta.Cwd == CanonicalRepoPath(cwd) &&
		meta.AgentHash == agentArgvHash(s.agentArgv)
}

// claimRetainedTerminal 尝试把上一轮成功完成时保留的同 issue 终端转成
// 本轮 active worker。默认复用；身份不匹配或已过期时不静默误用。
func (s *Scheduler) claimRetainedTerminal(ctx context.Context, key graph.Key, issueIID int, integration bool, termName, cwd string) (string, bool, error) {
	path := retainedMetaPath(s.repo.Root, s.specIID, issueIID, integration)
	meta, err := readRetainedMeta(path)
	if err != nil {
		return "", false, nil
	}
	id := terminalID{key: key, integration: integration}
	expired := !meta.CleanupAt.IsZero() && !time.Now().Before(meta.CleanupAt)
	record, recErr := s.sessions.Snapshot(ctx, meta.TerminalAddr)
	if recErr != nil || !record.Live {
		s.cancelRetained(id)
		_ = os.Remove(path)
		return "", false, nil
	}
	if expired {
		s.cancelRetained(id)
		if err := s.stopWorker(meta.TerminalAddr, meta.TerminalName); err != nil {
			return "", false, fmt.Errorf("clean expired retained terminal %s: %w", meta.TerminalName, err)
		}
		_ = os.Remove(path)
		s.runtime.clearAddr(key)
		return "", false, nil
	}
	if !retainedRecordMatches(record, meta) {
		return "", false, fmt.Errorf("retained terminal %s identity changed; refusing to reuse or replace it", meta.TerminalName)
	}
	if !s.staticRetainedMatch(meta, issueIID, integration, termName, cwd) {
		return "", false, fmt.Errorf("retained terminal %s cannot be reused after agent/cwd/name change; inspect it with hc attach %s", meta.TerminalName, meta.TerminalName)
	}
	s.cancelRetained(id)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", false, fmt.Errorf("activate retained terminal metadata %s: %w", path, err)
	}
	s.runtime.setAddr(key, meta.TerminalAddr, meta.TerminalName, integration)
	// 复用不是继续上一轮工作：界面上的开始时间/工作时长从本轮激活重算。
	s.runtime.start(key, time.Now())
	s.log.Info("reusing retained worker terminal", "key", key, "terminal", meta.TerminalName, "addr", meta.TerminalAddr)
	return meta.TerminalAddr, true, nil
}
