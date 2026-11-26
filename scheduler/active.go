package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// activeMetaVersion 是 active worker metadata 的 schema 版本。
	activeMetaVersion = 1
	// ActiveHeartbeatInterval 是监督进程更新 heartbeat 的间隔。状态迁移
	// 会立即写 metadata；这个 ticker 只证明进程仍在管理任务。
	ActiveHeartbeatInterval = time.Minute
	// ActiveStaleAfter 是 heartbeat 过期阈值：过期不代表一定能接管，
	// 还要结合 supervisor 进程是否存活，避免 scheduler 卡住时被双控。
	ActiveStaleAfter = 3 * time.Minute
)

// activeStage 是任务在 fastimpl 监督协议里的位置。booting/waiting_report
// 与可幂等重入的 integrating 支持自动接管；prompting 位于发送动作交界处，
// 与 needs_attention 等不明边界一样保守转人工，绝不冒险重复 prompt。
type activeStage string

const (
	activeStageStarting activeStage = "starting"
	activeStageClaiming activeStage = "claiming"
	activeStageClaimed  activeStage = "claimed"
	activeStageBooting  activeStage = "booting"
	// activeStagePrompting 表示已经决定发送 prompt，但无法知道崩溃发生在
	// 发送前还是发送后。这个阶段绝不自动重发，只能人工检查终端。
	activeStagePrompting      activeStage = "prompting"
	activeStageWaitingReport  activeStage = "waiting_report"
	activeStageIntegrating    activeStage = "integrating"
	activeStageNeedsAttention activeStage = "needs_attention"
)

// SupervisorMeta 是管理 worker 的 fastimpl/GUI 进程身份。PID 用于本机
// 存活判断；instance/executable/hostname 供人排查与跨机器辨认。
type SupervisorMeta struct {
	PID int `json:"pid"`
	// StartID 是操作系统给的进程启动 identity（Linux 上是 /proc stat 的
	// starttime tick）。PID 会被复用，单看 PID 不足以确认旧 supervisor
	// 还活着；只有 PID 与 StartID 同时匹配才算同一个进程。
	StartID     string    `json:"start_id,omitempty"`
	InstanceID  string    `json:"instance_id"`
	Executable  string    `json:"executable,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// ActiveTaskMeta 是运行中 worker 的接管元数据。它放在 lock 旁路并原子
// 更新：lock 仍只表示互斥，metadata 则描述“这个锁背后的工作如何继续”。
type ActiveTaskMeta struct {
	Version           int         `json:"version"`
	RepoDir           string      `json:"repo_dir"`
	WorkDir           string      `json:"work_dir"`
	TreeDir           string      `json:"tree_dir"`
	Worktree          string      `json:"worktree"`
	IntegrationDir    string      `json:"integration_dir"`
	ProjectID         int         `json:"project_id"`
	SpecIID           int         `json:"spec_iid"`
	IssueIID          int         `json:"issue_iid"`
	Branch            string      `json:"branch"`
	IntegrationBranch string      `json:"integration_branch"`
	BaseBranch        string      `json:"base_branch"`
	TerminalAddr      string      `json:"terminal_addr"`
	TerminalName      string      `json:"terminal_name"`
	TerminalCwd       string      `json:"terminal_cwd,omitempty"`
	TerminalPID       int         `json:"terminal_pid,omitempty"`
	TerminalArgv      []string    `json:"terminal_argv,omitempty"`
	HCBinary          string      `json:"hc_binary,omitempty"`
	HCSocket          string      `json:"hc_socket,omitempty"`
	ClaimToken        string      `json:"claim_token"`
	ReportToken       string      `json:"report_token"`
	Stage             activeStage `json:"stage"`
	PromptBaseline    uint64      `json:"prompt_baseline"`
	Attempts          int         `json:"attempts"`
	StartedAt         time.Time   `json:"started_at"`
	UpdatedAt         time.Time   `json:"updated_at"`
	// Integration* 记录 integrating 阶段内部的幂等恢复点。普通 merge/push
	// 可以安全重入；base 冲突 agent 只有在 token/baseline/prompt_sent 都
	// 完整时才能继续等待，绝不能重启第二个集成 agent。
	IntegrationPhase        string         `json:"integration_phase,omitempty"`
	IntegrationTerminalAddr string         `json:"integration_terminal_addr,omitempty"`
	IntegrationTerminalName string         `json:"integration_terminal_name,omitempty"`
	IntegrationTerminalCwd  string         `json:"integration_terminal_cwd,omitempty"`
	IntegrationTerminalPID  int            `json:"integration_terminal_pid,omitempty"`
	IntegrationTerminalArgv []string       `json:"integration_terminal_argv,omitempty"`
	IntegrationToken        string         `json:"integration_token,omitempty"`
	IntegrationBaseline     uint64         `json:"integration_baseline,omitempty"`
	IntegrationAttempts     int            `json:"integration_attempts,omitempty"`
	IntegrationPromptSent   bool           `json:"integration_prompt_sent,omitempty"`
	Supervisor              SupervisorMeta `json:"supervisor"`
}

const (
	integrationPhaseSyncingBase           = "syncing_base"
	integrationPhaseResolvingBaseConflict = "resolving_base_conflict"
	integrationPhaseMergingTask           = "merging_task"
	integrationPhasePushing               = "pushing"
)

// OwnerState 是另一个实例看到 metadata 时的处置结论。
type OwnerState int

const (
	// OwnerAdoptable：原 supervisor 已不存在，可以尝试接管。
	OwnerAdoptable OwnerState = iota
	// OwnerActive：heartbeat 新鲜且 supervisor 活着，只允许只读观察。
	OwnerActive
	// OwnerBlocked：supervisor 活着但 heartbeat 过期；它可能只是卡住，
	// 保守起见不接管，避免两个 scheduler 同时控制同一 worker。
	OwnerBlocked
)

func (s OwnerState) String() string {
	switch s {
	case OwnerActive:
		return "active"
	case OwnerBlocked:
		return "blocked"
	}
	return "adoptable"
}

// OwnerState 按 heartbeat 与 supervisor 进程存活状态分类。进程死亡比
// heartbeat 更强：只要确定旧 supervisor 不在，就不必等满 3 分钟。
func (m ActiveTaskMeta) OwnerState(now time.Time) OwnerState {
	if !supervisorAlive(m.Supervisor) {
		return OwnerAdoptable
	}
	if now.Sub(m.Supervisor.HeartbeatAt) <= ActiveStaleAfter {
		return OwnerActive
	}
	return OwnerBlocked
}

// supervisorAlive 同时检查 PID 与进程启动 identity。旧 metadata 没有
// StartID 时退回 PID 检查；有 StartID 但当前系统无法读取 identity 时
// 保守视为活着，绝不因为“无法确认”而冒险接管。
func supervisorAlive(sup SupervisorMeta) bool {
	if !processAlive(sup.PID) {
		return false
	}
	if sup.StartID == "" {
		return true
	}
	current := processStartID(sup.PID)
	if current == "" {
		return true
	}
	return current == sup.StartID
}

// ActiveRecoveryState 是启动扫描看到一个 spec 时的处置结论。
type ActiveRecoveryState int

const (
	// ActiveRecoveryNone 表示没有相关 active metadata。
	ActiveRecoveryNone ActiveRecoveryState = iota
	// ActiveRecoveryObserve 表示原 supervisor 活着且 heartbeat 新鲜：只读观察。
	ActiveRecoveryObserve
	// ActiveRecoveryAdoptable 表示原 supervisor 已死，且任务停在可无缝接管阶段。
	ActiveRecoveryAdoptable
	// ActiveRecoveryBlocked 表示状态不够安全：心跳卡住、阶段不明或 schema 不支持。
	ActiveRecoveryBlocked
)

func (s ActiveRecoveryState) String() string {
	switch s {
	case ActiveRecoveryObserve:
		return "observe"
	case ActiveRecoveryAdoptable:
		return "adoptable"
	case ActiveRecoveryBlocked:
		return "blocked"
	}
	return "none"
}

func recoverableStage(stage activeStage) bool {
	return stage == activeStageBooting || stage == activeStageWaitingReport || stage == activeStageIntegrating
}

// RecoveryState 合成单个任务的 owner 与业务阶段。即使 supervisor 已死，
// integrating/needs_attention 也不能盲接：它可能处在 merge/close 的一半。
func (m ActiveTaskMeta) RecoveryState(now time.Time) ActiveRecoveryState {
	switch m.OwnerState(now) {
	case OwnerActive:
		return ActiveRecoveryObserve
	case OwnerBlocked:
		return ActiveRecoveryBlocked
	}
	if !recoverableStage(m.Stage) {
		return ActiveRecoveryBlocked
	}
	if m.Stage == activeStageIntegrating && m.IntegrationPhase == integrationPhaseResolvingBaseConflict &&
		(m.IntegrationTerminalAddr == "" || m.IntegrationTerminalName == "" || m.IntegrationTerminalCwd == "" ||
			m.IntegrationTerminalPID <= 0 || len(m.IntegrationTerminalArgv) == 0 || m.IntegrationToken == "" || !m.IntegrationPromptSent) {
		return ActiveRecoveryBlocked
	}
	return ActiveRecoveryAdoptable
}

// ActiveSpecChoice 是启动扫描对一个 spec 的决定。
type ActiveSpecChoice struct {
	SpecIID int
	State   ActiveRecoveryState
	Tasks   []ActiveTaskMeta
	// UpdatedAt 是同组 metadata 的最近更新时间，用于多个 spec 中选择最新现场。
	UpdatedAt time.Time
}

// ChooseActiveSpec 从扫描结果里选择最应该呈现/恢复的 spec。活跃 supervisor
// 优先于可接管的旧现场：宁可只读观察，也不能让新实例绕过仍在管理工作的
// 原进程。多个 spec 都没有活跃 owner 时选择最近更新的一组。
func ChooseActiveSpec(metas []ActiveTaskMeta, now time.Time) ActiveSpecChoice {
	groups := make(map[int][]ActiveTaskMeta)
	for _, meta := range metas {
		groups[meta.SpecIID] = append(groups[meta.SpecIID], meta)
	}
	choices := make([]ActiveSpecChoice, 0, len(groups))
	for spec, tasks := range groups {
		choice := ActiveSpecChoice{SpecIID: spec, State: ActiveRecoveryAdoptable, Tasks: tasks}
		for _, task := range tasks {
			if task.UpdatedAt.After(choice.UpdatedAt) {
				choice.UpdatedAt = task.UpdatedAt
			}
			switch task.RecoveryState(now) {
			case ActiveRecoveryObserve:
				choice.State = ActiveRecoveryObserve
			case ActiveRecoveryBlocked:
				if choice.State != ActiveRecoveryObserve {
					choice.State = ActiveRecoveryBlocked
				}
			}
		}
		choices = append(choices, choice)
	}
	if len(choices) == 0 {
		return ActiveSpecChoice{State: ActiveRecoveryNone}
	}
	// 活跃 supervisor 所在的 spec 永远排最前；否则按最近现场排序。
	sort.Slice(choices, func(i, j int) bool {
		if (choices[i].State == ActiveRecoveryObserve) != (choices[j].State == ActiveRecoveryObserve) {
			return choices[i].State == ActiveRecoveryObserve
		}
		if !choices[i].UpdatedAt.Equal(choices[j].UpdatedAt) {
			return choices[i].UpdatedAt.After(choices[j].UpdatedAt)
		}
		return choices[i].SpecIID > choices[j].SpecIID
	})
	return choices[0]
}

func activeRepoHash(repoDir string) string {
	sum := sha256.Sum256([]byte(CanonicalRepoPath(repoDir)))
	return hex.EncodeToString(sum[:])
}

// ActiveMetaPath 是某 issue 的 active metadata 旁路文件。
func ActiveMetaPath(repoDir string, iid int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("fastimpl-%s-%d.active.json", activeRepoHash(repoDir), iid))
}

func activeMetaPattern(repoDir string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("fastimpl-%s-*.active.json", activeRepoHash(repoDir)))
}

// writeFileAtomic0600 用同目录随机临时文件做原子替换。metadata 放在共享
// temp 目录，固定 tmp 名会被符号链接/预创建攻击利用；CreateTemp 不跟随
// 攻击者预置的路径，临时文件本身也始终是 0600。
func writeFileAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

// writeActiveMetaFile 是测试可替换的落盘出口；生产路径始终是随机临时
// 文件 + rename 的原子写。
var writeActiveMetaFile = writeFileAtomic0600

// writeActiveMeta 原子写 metadata，读者要么看到旧完整版本，要么看到新
// 完整版本；heartbeat 每分钟一次，绝不能写出半份 JSON 误导接管。
func writeActiveMeta(path string, meta ActiveTaskMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeActiveMetaFile(path, append(data, '\n'))
}

func readActiveMeta(path string) (ActiveTaskMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ActiveTaskMeta{}, err
	}
	var meta ActiveTaskMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return ActiveTaskMeta{}, err
	}
	if meta.Version != activeMetaVersion {
		return ActiveTaskMeta{}, fmt.Errorf("unsupported active metadata version %d", meta.Version)
	}
	return meta, nil
}

// ScanActiveTasksDetailed 扫描本仓库全部 active metadata，并单独列出匹配
// active pattern 但不能解析、版本不符或仓库身份不符的文件。调用方可以决定
// 是跳过（纯展示）还是保守阻塞（任何会影响 worker 的决策）。
func ScanActiveTasksDetailed(repoDir string) (tasks []ActiveTaskMeta, invalid []string, err error) {
	matches, err := filepath.Glob(activeMetaPattern(repoDir))
	if err != nil {
		return nil, nil, err
	}
	tasks = make([]ActiveTaskMeta, 0, len(matches))
	for _, path := range matches {
		meta, readErr := readActiveMeta(path)
		if readErr != nil || CanonicalRepoPath(meta.RepoDir) != CanonicalRepoPath(repoDir) {
			invalid = append(invalid, path)
			continue
		}
		tasks = append(tasks, meta)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].SpecIID != tasks[j].SpecIID {
			return tasks[i].SpecIID < tasks[j].SpecIID
		}
		return tasks[i].IssueIID < tasks[j].IssueIID
	})
	return tasks, invalid, nil
}

// ScanActiveTasks 扫描本仓库全部有效 active metadata。坏文件仍由
// Detailed 变体报告；这个兼容入口只返回可解析任务，供不改变语义的
// 展示/测试路径使用。
func ScanActiveTasks(repoDir string) ([]ActiveTaskMeta, error) {
	tasks, _, err := ScanActiveTasksDetailed(repoDir)
	return tasks, err
}

// activeWriter 串行化一个任务的 metadata 更新。初始写入失败必须 fail-closed；
// 后续更新是否可继续由调用方按所处副作用边界决定，错误不会在这里被吞掉。
type activeWriter struct {
	mu     sync.Mutex
	path   string
	meta   ActiveTaskMeta
	closed bool
}

func newActiveWriter(path string, meta ActiveTaskMeta) (*activeWriter, error) {
	w := &activeWriter{path: path, meta: meta}
	if err := w.writeLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *activeWriter) writeLocked() error {
	w.meta.UpdatedAt = time.Now()
	if w.meta.Supervisor.HeartbeatAt.IsZero() {
		w.meta.Supervisor.HeartbeatAt = w.meta.UpdatedAt
	}
	return writeActiveMeta(w.path, w.meta)
}

// update 修改 metadata 并立即落盘；状态迁移不等下一分钟 heartbeat。
func (w *activeWriter) update(fn func(*ActiveTaskMeta)) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	fn(&w.meta)
	w.meta.Supervisor.HeartbeatAt = time.Now()
	return w.writeLocked()
}

// heartbeat 只证明 supervisor 还在管理任务，不改变业务状态。
func (w *activeWriter) heartbeat() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.meta.Supervisor.HeartbeatAt = time.Now()
	return w.writeLocked()
}

func (w *activeWriter) remove() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func currentSupervisor(instanceID string) SupervisorMeta {
	exe, _ := os.Executable()
	host, _ := os.Hostname()
	return SupervisorMeta{PID: os.Getpid(), StartID: processStartID(os.Getpid()), InstanceID: instanceID, Executable: filepath.Base(exe), Hostname: host, HeartbeatAt: time.Now()}
}

// startActiveHeartbeat 每 ActiveHeartbeatInterval 更新一次 metadata。
// 测试可用 Scheduler.activeHeartbeat 缩短；ctx 结束时 goroutine 退出。
func (s *Scheduler) startActiveHeartbeat(ctx context.Context, w *activeWriter) {
	interval := s.activeHeartbeat
	if interval <= 0 {
		interval = ActiveHeartbeatInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.heartbeat(); err != nil {
					s.log.Warn("active worker heartbeat failed", "path", w.path, "err", err)
				}
			}
		}
	}()
}
