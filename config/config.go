// Package config 汇总一次运行所需的全部设置：TOML 配置文件、
// 环境变量、命令行覆盖，优先级依次升高。配置文件按 files.Resolve
// 约定查找：<work-dir>/fastimpl.toml > <exe 同目录>/fastimpl.toml；
// 两处都没有时把 DefaultConfig 播种到 exe 目录。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/yangjuncode/fastimpl/files"
)

// DefaultFile 是默认配置文件名，先在 work-dir 查找，再在 exe 目录查找。
const DefaultFile = "fastimpl.toml"

// DefaultConfig 是配置文件都不存在时播种到 exe 目录的默认内容。
const DefaultConfig = `# fastimpl 配置（TOML）
# 命令行参数（-repo-dir/-tree-dir/-work-dir/-j/-template）优先于本文件。
# <work-dir>/fastimpl.toml 是覆盖层：只写要改动的项，未写的项继承本文件。

# parallelism = 8                  # 同时运行的 worker 上限（默认 max(NumCPU-1, 8)）
# retries = 8                      # 单个任务首次尝试之外的续写重试次数
# forge = "gitlab"                 # 上游 forge：gitlab|github；默认按 origin
#                                # 主机名推断，推断不出用 glab 探测，最后按 gitlab
# repo_dir = "/abs/path/to/repo"   # 目标仓库主 checkout（也可 -repo-dir；
#                                # 都不给时从 work-dir 向上/下各搜 8 级取唯一仓库）
# tree_dir = "/abs/path"           # worktree 父目录（也可 -tree-dir；都不给时
#                                # 推断：既有 worktree 统一父目录 > <repo_dir>/.worktrees）
# agent_cmd = "devin --permission-mode dangerous"  # worker 的 agent 命令行
#                                                 # （按 shell 规则拆分）；
#                                                 # 缺省即此
# integration = "branch"           # 覆盖默认集成分支 issue-<spec>-integration
# require_review = true            # worker 提示词是否强制 leader + 第三方
#                                # （fusion-3rd-reviewer-swe2max）双审（默认 false）
# hc_socket = "/run/user/1000/hc.sock"  # herdr-core socket（与 hc_binary 二选一）
# hc_binary = "hc"                      # 或经 hc connect 连接
#
# 安全约定：FASTIMPL_LLM_* 环境变量不会传给任何子进程
# （glab/gh/worker 终端的进程环境都会被清理）。
`

const (
	// DefaultRetries 是单个任务在首次尝试之外的重试次数。
	DefaultRetries = 8
	// LLM 相关环境变量名：不会读入配置，只用于把它们从子进程
	// 环境中清除，防止凭据泄漏给 glab/gh/agent 进程。
	LLMAPIKeyEnv  = "FASTIMPL_LLM_API_KEY"
	LLMBaseURLEnv = "FASTIMPL_LLM_BASE_URL"
	LLMModelEnv   = "FASTIMPL_LLM_MODEL"
)

// exeDir 是可测试的接缝：返回可执行文件所在目录。
var exeDir = files.ExeDir

// DefaultResumeBudget 是任务暂停后等待人工推进的默认时限。
const DefaultResumeBudget = 30 * time.Minute

// 支持的上游 forge 类型。
const (
	// ForgeGitLab 由 glab CLI 读写 issue。
	ForgeGitLab = "gitlab"
	// ForgeGitHub 由 gh CLI 读写 issue（适配器骨架在 github 包）。
	ForgeGitHub = "github"
)

// Config 是一次运行的全部设置。
type Config struct {
	// RepoDir 是目标仓库主 checkout 的绝对路径。
	RepoDir string
	// TreeDir 是 worktree 的父目录；缺省由 worktree.ResolveTreeDir
	// 推断（既有 worktree 统一父目录 > <repo-dir>/.worktrees）。
	TreeDir string
	// WorkDir 是 fastimpl 工作目录；缺省为启动时的当前目录。
	WorkDir string
	// Parallelism 是同时运行的 worker 上限。
	Parallelism int
	// Retries 是单个任务在首次尝试之外的重试次数，0 表示只尝试一次。
	Retries int
	// AgentCmd 是 worker 的 agent 命令行（含参数，按 shell 规则拆分）。
	// 空时使用 devin --permission-mode dangerous；配置后原样使用，
	// 由用户自带模型、权限等参数。
	AgentCmd string
	// Integration 非空时覆盖默认集成分支 issue-<spec>-integration。
	Integration string
	// HcBinary 与 HcSocket 二选一：前者执行 `hc connect`，后者直连 Unix socket。
	HcBinary string
	HcSocket string
	// Forge 是上游 forge 类型（ForgeGitLab|ForgeGitHub）；空表示按
	// origin 主机名推断，推断不出按 ForgeGitLab。
	Forge string
	// RequireReview 控制 worker 提示词是否写入"leader + 第三方
	// 独立审核全部通过"的硬性要求（默认 true）。
	RequireReview bool
	// InstanceID 是本机稳定标识，跨机器认领 issue 时写进评论标记。
	// 只从 exe 同目录的 fastimpl.toml 读取，工作目录与 -config
	// 都不能覆盖它。
	InstanceID string
	// SpecIID 是本次运行的 spec issue IID，由命令行给出。
	SpecIID int
	// ResumeBudget 是暂停后等待用户推进的时限。
	ResumeBudget time.Duration
}

// rawConfig 区分"未配置"与显式零值，使 retries = 0 不被默认值覆盖。
type rawConfig struct {
	Parallelism   *int    `toml:"parallelism"`
	Retries       *int    `toml:"retries"`
	RepoDir       *string `toml:"repo_dir"`
	TreeDir       *string `toml:"tree_dir"`
	AgentCmd      *string `toml:"agent_cmd"`
	Integration   *string `toml:"integration"`
	RequireReview *bool   `toml:"require_review"`
	HcBinary      *string `toml:"hc_binary"`
	HcSocket      *string `toml:"hc_socket"`
	Forge         *string `toml:"forge"`
	InstanceID    *string `toml:"instance_id"`
}

// defaultParallelism 是未配置 parallelism 时的并发上限。
func defaultParallelism() int {
	return max(runtime.NumCPU()-1, 8)
}

// defaultAgentArgv 是未配置 agent_cmd 时的 worker 命令行：devin
// 需要 --permission-mode dangerous 才能自主改代码。
var defaultAgentArgv = []string{"devin", "--permission-mode", "dangerous"}

// AgentArgv 返回 worker 的 agent 命令 argv：agent_cmd 已配置时按
// shell 规则拆分，否则使用 defaultAgentArgv。
func (c Config) AgentArgv() ([]string, error) {
	if strings.TrimSpace(c.AgentCmd) != "" {
		return splitCmd(c.AgentCmd)
	}
	return append([]string{}, defaultAgentArgv...), nil
}

// splitCmd 按简化 shell 规则拆分命令行：空格分词、单双引号、
// 反斜杠转义。不支持变量展开与通配。
func splitCmd(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	var quote rune // 0 无引号，' 或 " 引号内
	escaped := false
	hasToken := false // 引号包的空串也是有效参数
	flush := func() {
		argv = append(argv, cur.String())
		cur.Reset()
		hasToken = false
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
			hasToken = true
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			hasToken = true
		case r == '\'' || r == '"':
			quote = r
			hasToken = true
		case r == ' ' || r == '\t':
			if hasToken {
				flush()
			}
		default:
			cur.WriteRune(r)
			hasToken = true
		}
	}
	if escaped {
		cur.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("agent_cmd %q has an unterminated quote", s)
	}
	if hasToken {
		flush()
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("agent_cmd %q is empty", s)
	}
	return argv, nil
}

// Load 读取配置：内置默认值 → exe 目录 fastimpl.toml（全局默认层）
// → 覆盖层（-config 显式文件，否则 <work-dir>/fastimpl.toml）→ 环境变量。
// 覆盖层只写要改动的项即可，未写的项继承全局默认层。
// -config 显式指定的文件必须存在；未指定时若两层都没有文件，把
// DefaultConfig 播种到 exe 目录作为全局默认。
// 命令行覆盖（-repo-dir 等）由调用方在 Load 之后叠加。
func Load(workDir, path string) (Config, error) {
	dir, err := exeDir()
	if err != nil {
		return Config{}, err
	}
	base, baseFound, err := readLayer(filepath.Join(dir, DefaultFile), false)
	if err != nil {
		return Config{}, err
	}
	override := path
	if override == "" {
		override = filepath.Join(workDir, DefaultFile)
	}
	over, overFound, err := readLayer(override, path != "")
	if err != nil {
		return Config{}, err
	}
	if path == "" && !baseFound && !overFound {
		if err := SeedDefaultConfig(filepath.Join(dir, DefaultFile)); err != nil {
			return Config{}, err
		}
	}
	cfg := Config{
		Parallelism:   defaultParallelism(),
		Retries:       DefaultRetries,
		WorkDir:       workDir,
		RequireReview: false,
		ResumeBudget:  DefaultResumeBudget,
	}
	applyRaw(&cfg, base)
	applyRaw(&cfg, over)
	// 机器标识只认 exe 同目录的全局层：工作目录与 -config 都不能覆盖，
	// 否则同一台机器会因项目不同而变成两个实例。
	if base.InstanceID != nil {
		id := strings.TrimSpace(*base.InstanceID)
		if !validInstanceID(id) {
			return Config{}, fmt.Errorf("invalid instance_id %q in %s: use only [A-Za-z0-9._-], at most %d chars",
				id, filepath.Join(dir, DefaultFile), maxInstanceIDLen)
		}
		cfg.InstanceID = id
	}
	scrubSecretEnv()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// readLayer 解码一层配置文件。文件缺失时：required 为真报错，
// 否则返回 found=false 且不视为错误。
func readLayer(path string, required bool) (raw rawConfig, found bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !required {
			return rawConfig{}, false, nil
		}
		return rawConfig{}, false, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := toml.Unmarshal(data, &raw); err != nil {
		return rawConfig{}, true, fmt.Errorf("decode config %s: %w", path, err)
	}
	return raw, true, nil
}

// applyRaw 把配置文件中的显式设置叠加到默认值上。
func applyRaw(cfg *Config, raw rawConfig) {
	if raw.Parallelism != nil {
		cfg.Parallelism = *raw.Parallelism
	}
	if raw.Retries != nil {
		cfg.Retries = *raw.Retries
	}
	if raw.RepoDir != nil {
		cfg.RepoDir = *raw.RepoDir
	}
	if raw.TreeDir != nil {
		cfg.TreeDir = *raw.TreeDir
	}
	if raw.AgentCmd != nil {
		cfg.AgentCmd = *raw.AgentCmd
	}
	if raw.Integration != nil {
		cfg.Integration = *raw.Integration
	}
	if raw.RequireReview != nil {
		cfg.RequireReview = *raw.RequireReview
	}
	if raw.HcBinary != nil {
		cfg.HcBinary = *raw.HcBinary
	}
	if raw.HcSocket != nil {
		cfg.HcSocket = *raw.HcSocket
	}
	if raw.Forge != nil {
		cfg.Forge = *raw.Forge
	}
}

// scrubSecretEnv 把 LLM 密钥变量从本进程环境中移除，使 fastimpl
// 启动的子进程无法继承它（worker 启动时也会显式清空）。
func scrubSecretEnv() {
	for _, name := range []string{LLMAPIKeyEnv, LLMBaseURLEnv, LLMModelEnv} {
		_ = os.Unsetenv(name)
	}
}

// ScrubbedEnv 返回去掉 FASTIMPL_LLM_* 后的进程环境，
// 供 glab/gh 等子进程继承，避免凭据泄漏。
func ScrubbedEnv() []string {
	out := os.Environ()
	keep := out[:0]
	for _, kv := range out {
		if strings.HasPrefix(kv, "FASTIMPL_LLM_") {
			continue
		}
		keep = append(keep, kv)
	}
	return keep
}

// Validate 拒绝自相矛盾或无法运行的设置。
func (c Config) Validate() error {
	if c.Parallelism < 1 {
		return fmt.Errorf("parallelism must be at least 1, got %d", c.Parallelism)
	}
	if c.Retries < 0 {
		return fmt.Errorf("retries must not be negative, got %d", c.Retries)
	}
	if c.HcBinary != "" && c.HcSocket != "" {
		return errors.New("hc_binary and hc_socket are mutually exclusive")
	}
	if c.RepoDir != "" && !filepath.IsAbs(c.RepoDir) {
		return fmt.Errorf("repo_dir must be an absolute path, got %q", c.RepoDir)
	}
	if c.TreeDir != "" && !filepath.IsAbs(c.TreeDir) {
		return fmt.Errorf("tree_dir must be an absolute path, got %q", c.TreeDir)
	}
	switch c.Forge {
	case "", ForgeGitLab, ForgeGitHub:
	default:
		return fmt.Errorf("forge must be %q or %q, got %q", ForgeGitLab, ForgeGitHub, c.Forge)
	}
	if c.InstanceID != "" && !validInstanceID(c.InstanceID) {
		return fmt.Errorf("instance_id %q must contain only [A-Za-z0-9._-] and be at most %d chars",
			c.InstanceID, maxInstanceIDLen)
	}
	return nil
}

// IntegrationBranch 返回 spec 使用的集成分支名。
func (c Config) IntegrationBranch(spec int) string {
	if c.Integration != "" {
		return c.Integration
	}
	return IntegrationBranchName(spec)
}

// IntegrationBranchName 是 spec issue 默认的集成分支名。
func IntegrationBranchName(spec int) string {
	return "issue-" + strconv.Itoa(spec) + "-integration"
}
