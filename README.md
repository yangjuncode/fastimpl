# fastimpl

把一个 spec issue 的全部子任务（含跨层级的 blocked-by 依赖）自动构建成
依赖图，按依赖关系并行调度一批 AI agent，每个任务在独立的 git worktree
中实现，完成后串行合入集成分支、推送、关闭 issue——一条命令完成原来
逐个手工跑 implement 技能的整个流程。

上游同时支持 **GitLab**（`glab`）与 **GitHub**（`gh`）；agent 进程由
[herdr-core](https://github.com/yangjuncode/hcgo)（hc）管理，运行中随时
可以 `hc attach` 进去查看或与 agent 对话。

## 目录

- [工作原理](#工作原理)
  - [总体流程](#总体流程)
  - [依赖图](#依赖图)
  - [调度模型](#调度模型)
  - [分支与 worktree 约定](#分支与-worktree-约定)
  - [worker 生命周期与完成协议](#worker-生命周期与完成协议)
  - [集成、冲突与上游同步](#集成冲突与上游同步)
  - [状态刷新与失败处理](#状态刷新与失败处理)
- [使用说明](#使用说明)
  - [前置要求](#前置要求)
  - [安装](#安装)
  - [快速开始](#快速开始)
  - [命令行参数](#命令行参数)
  - [配置文件 fastimpl.toml](#配置文件-fastimpltoml)
  - [提示词模板 fastimpl.prompt.md](#提示词模板-fastimplpromptmd)
  - [目录解析约定](#目录解析约定)
  - [观察与排查](#观察与排查)
  - [中断与重跑](#中断与重跑)
- [安全约定](#安全约定)
- [项目结构](#项目结构)
- [开发与测试](#开发与测试)

## 工作原理

### 总体流程

```
┌─ 解析阶段（只读）─────────────────────────────────────────────┐
│ work-dir → fastimpl.toml（分层覆盖）→ repo-dir → origin        │
│ → forge 判定 → 建依赖图（-print/-json 到此为止）               │
└──────────────────────────────────────────────────────────────┘
┌─ 准备阶段 ────────────────────────────────────────────────────┐
│ tree-dir → 确保集成分支 worktree → 同步 base → push → 确保 MR │
└──────────────────────────────────────────────────────────────┘
┌─ 调度阶段（写操作开始）───────────────────────────────────────┐
│ 就绪即派：issue 状态预检 → claim(@me) → 建任务 worktree        │
│ → 启动 hc 终端 → 发提示词 → 轮询轮次 → 检测完成标记            │
│ → 串行集成（sync base → merge → push）→ close issue           │
│ → 停终端 → 删 worktree/分支 → 立即释放后继任务                │
└──────────────────────────────────────────────────────────────┘
```

### 依赖图

`graph.Build` 以 spec issue 为根做 **BFS 传递闭包**：

- **种子**：spec 的直接子任务（GitLab 只取 task 类型的 work item；
  GitHub 用原生 sub-issues API）。
- **展开**：每个节点取其 **blocked-by 关联**（GitLab work item links /
  GitHub dependencies API），另解析正文中的 `Blocked by: #n` 行作为降级
  来源。新发现的节点继续展开，直到闭包收敛。
- **节点入图规则**：所有出现在闭包里的 issue 都入图——包括**已关闭**的
  （视为已完成，仅用于展示）和**跨项目**的（只作依赖 gate，见下）。
- **环检测**：blocked-by 形成环时报错退出，不会进入调度。

节点的关键身份是 `graph.Key{ProjectID, IID}`：GitLab 上是项目 ID +
项目内 IID；GitHub 上是仓库的 database id + issue number。这样跨仓库
依赖（GitHub sub-issue 可以跨同 owner 仓库、GitLab 跨项目）都能正确定位。

`-print` 的输出按 **Kahn 拓扑分层**打印：level 0 是无前置任务，level n
的任务全部前置都在更低的层——同一层内可并行。

### 调度模型

`scheduler.Run` 是一个事件循环（500ms tick），语义是**就绪即派、
完成即放**，不是"按波次等待"：

- **就绪条件**（`Graph.Ready`）：未关闭 + `Pending` + 所有直接前置
  `Closed` 或 `Integrated`。**只取与 spec 同项目的节点**——跨项目节点
  只作为 gate：它开放时阻塞后继，它被（外部）关闭时释放后继，但
  fastimpl 不会为它建 worktree、claim 或关闭它。
- **并发上限**：`parallelism`（默认 `max(NumCPU-1, 8)`，`-j` 可覆盖）。
  有空槽就立即派下一个就绪任务；一个任务 `Integrated` 的瞬间，依赖它
  的下一批任务立刻被释放，不等同波次其他人完成。
- **每 10 分钟**周期重读所有节点在 forge 上的开关状态
  （`refreshStates`）：运行期间被外部关闭的依赖无需等任何任务完成即可
  释放后继；被重新打开的恢复阻塞。
- **终止**：
  - 全部同项目工作项 `Integrated`/`Closed`/失败 且无在跑 → 正常返回；
  - 无就绪且无在跑但图未完成 → `scheduler stalled` 错误，列出
    needs-attention 项与被开放跨项目前置卡住的项；
  - `ctx` 取消（Ctrl+C/SIGTERM）→ 走 defer 清理后返回。

### 分支与 worktree 约定

| 对象 | 命名 | 目录 | 生命周期 |
|---|---|---|---|
| 集成分支 | `issue-<spec>-integration`（可被 `integration` 配置覆盖）；复用时也接受 `issue-<spec>`、`issue-<spec>-<描述>` | `<tree-dir>/<分支名>` | 运行开始时确保存在：已有则复用，否则按 `main → master → origin/HEAD` 顺序选基点创建 |
| 任务分支 | `issue-<iid>`（新建）；复用时接受 `issue-<iid>-<描述>` | `<tree-dir>/<分支名>` | 从集成分支创建；集成成功后与 worktree 一起删除 |
| hc 终端 | `<repo名>-<分支名>`，如 `bf9100-issue-9`、集成终端 `bf9100-issue-8-integration` | — | repo 名取 origin 项目路径最后一段，多项目并行不撞名 |

所有 git 操作都在 worktree 中进行，**主 checkout 不被触碰**——你可以
照常使用自己的仓库目录。

### worker 生命周期与完成协议

每个就绪任务在独立 goroutine 中走完完整生命周期：

1. **预检**：claim 前重读 issue 现场状态（建图快照可能已过期）——
   已关闭 → 跳过并标记 `Closed`；已指派给他人 → `NeedsAttention`，
   不抢占。
2. **领取**：`glab issue update --assignee @me` / `gh issue edit
   --add-assignee @me`。
3. **建 worktree**：`git worktree add <dir> -b issue-<iid> <集成分支>`。
4. **启动 hc 终端**：在任务 worktree 中运行 `agent_cmd`（默认 `devin
   [--model …]`），终端名 `<repo>-<分支>`，日志立即打印
   `worker running key=<proj!iid> terminal=<名> addr=<地址>`。
5. **等启动**：轮询 OSC 7501 状态到 idle/done（上限 5 分钟）。
6. **发提示词**：渲染模板 `{issue-no}` `{spec-no}` `{token}`（token 是
   本任务随机的报告令牌）后发送。
7. **等一轮结束**：轮询到轮次前进且状态 idle/done（上限 90 分钟）。
8. **判定完成**：读终端屏幕，找本任务令牌的 **`FASTIMPL-READY <token>`**
   行。fastimpl **不做任何 LLM 判定**——完成与否只看这一个标记，
   提交以任务 worktree 的 `git rev-parse HEAD` 为准，不信 agent 自报。
   - 没有标记 → 发送"继续完成并输出标记"的续写提示词，回第 7 步；
     重试次数用满（`retries`，默认 8）仍无标记 → `NeedsAttention`。
   - 等轮次本身出错（超时、终端 error/退出）→ 直接 `NeedsAttention`，
     不消耗续写次数。

### 集成、冲突与上游同步

对集成分支的所有写操作由一把全局锁串行化（`tryMerge`），顺序固定：

```
SyncBase(origin/<base>)  →  merge --no-ff <任务分支>  →  push <集成分支>
```

- **上游同步**：每次集成前先 `fetch + merge origin/<base>` 把 base 分支
  （默认 main，缺失则 master/origin/HEAD）的更新合进集成分支——后续新
  建的任务 worktree 也自动带上最新上游。运行开始时（`EnsureIntegration`
  之后）同样先同步一次，保证复用的旧集成分支追平上游。
- **任务分支冲突**：merge 冲突 → 收集冲突文件、abort，**发提示词给该
  任务的 agent**，让它在自己的 worktree 里 `git merge <集成分支>` 解决
  冲突并提交，再输出 `FASTIMPL-READY <token>`；标记出现后重试干净合并
  （此时是快进或自动可合）。
- **base 同步冲突**：冲突由**集成分支专属 agent** 解决——懒启动一个
  终端（`<repo>-issue-<spec>-integration`，cwd 为集成 worktree），让它
  执行 `git merge origin/<base>`、解决冲突并提交（明确禁止 reset/
  rebase 丢弃已有成果），标记出现后重跑 `SyncBase` 验证。重试耗尽则
  置 `baseStuck` 跳过后续同步（任务合并照常），终端保留供排查。
- **推送与 MR**：启动时集成分支就位后立即 `push -u` 并 `EnsureMR`
  （GitLab MR / GitHub PR，目标为 base 分支）；之后每次集成成功再推送，
  MR 上的提交自然累积，可随时在页面上查看整体进度。

### 状态刷新与失败处理

- 任务集成成功 → `Integrated`：close issue → 停 hc 终端 → 删 worktree
  和任务分支 → 释放后继。
- 任务失败 → `NeedsAttention`：**保留 hc 终端和 worktree**，在 issue 上
  追加一条说明评论（`forge.Note`，含 `hc attach <终端名>` 提示），
  日志同样打印 attach 命令。人工修好（或发现只是误报）后可重新运行
  fastimpl：已 `Integrated`/`Closed` 的不会重做，needs-attention 的
  issue 仍未关闭、可再次调度（分支已存在时复用）。
- `NeedsAttention` 不是终态阻塞：其他不依赖它的任务照常并行推进；
  只有当只剩它被卡住时才会走到 `stalled` 错误。

## 使用说明

### 前置要求

- **Go** ≥ 1.27（编译用）。
- **`glab`**（GitLab）或 **`gh`**（GitHub）：已登录且对目标仓库有
  issue 读写、MR/PR 创建权限。
- **`hc`**（herdr-core CLI）：管理 agent 终端；或一个已在运行的
  herdr-core socket（`hc_socket`）。
- **agent CLI**：默认 `devin`；也可通过 `agent_cmd` 换成 codex、claude
  等任何能接受文本提示词并按协议输出标记的命令行 agent。
- 目标仓库的 origin 指向对应的 GitLab/GitHub 主机。

### 安装

```bash
go install github.com/yangjuncode/fastimpl/cmd/fastimpl@latest
# 或本地构建
go build -o fastimpl ./cmd/fastimpl
```

### 快速开始

```bash
cd /path/to/repo            # work-dir 默认就是当前目录
fastimpl -init              # 把配置与模板文件生成到当前目录，按需修改
fastimpl -print 42          # 先看 spec issue #42 的依赖图，不写任何东西
fastimpl 42                 # 正式运行
```

`-init` 把 `fastimpl.toml` 与 `fastimpl.prompt.md` 生成到 work-dir：exe 同
目录已有同名文件就**复制**它（全局层已定制的内容会带过来），没有则写
内置默认内容；已存在的文件不覆盖、只提示跳过。也可以不跑 `-init`——
首次正常运行时默认文件会自动播种到 exe 同目录。

### 命令行参数

```
usage: fastimpl [-init] [-print] [-json file] [-j N] [-config path]
        [-template path] [-repo-dir dir] [-tree-dir dir]
        [-work-dir dir] <spec-issue-iid>
```

| 参数 | 说明 |
|---|---|
| `-init` | 把 fastimpl.toml / fastimpl.prompt.md 生成到 work-dir 后退出（不接 spec IID，已有文件不覆盖） |
| `-print` | 只打印依赖图（按拓扑分层）后退出，不做任何写操作 |
| `-json <file>` | 把完整依赖图（含 open/closed）写到 JSON 文件；可与 `-print` 同用 |
| `-j <N>` | 并发 worker 上限；0 表示用配置值 |
| `-config <path>` | 显式指定 fastimpl.toml |
| `-template <path>` | 显式指定提示词模板 |
| `-repo-dir <dir>` | 目标仓库主 checkout；缺省自动定位（见下） |
| `-tree-dir <dir>` | worktree 父目录；缺省自动推断（见下） |
| `-work-dir <dir>` | fastimpl 工作目录；默认当前目录 |

### 配置文件 fastimpl.toml

配置文件**分层覆盖**：命令行 flag > `<work-dir>/fastimpl.toml` >
`<exe 同目录>/fastimpl.toml` > 内置默认值。work-dir 下的文件是覆盖层
——只写要改动的项，其余继承 exe 目录的默认配置。两处文件都不存在时
把默认内容播种到 exe 目录。

```toml
# parallelism = 8            # 同时运行的 worker 上限（默认 max(NumCPU-1, 8)）
# retries = 8                # 单任务在首次尝试之外的续写重试次数
# forge = "gitlab"           # gitlab|github；缺省按 origin 主机名推断，
                             # 推断不出用 glab/gh 探测，最后按 gitlab
# repo_dir = "/abs/path"     # 目标仓库主 checkout
# tree_dir = "/abs/path"     # worktree 父目录
# devin_model = "swe-2-medium"          # devin --model 参数
# agent_cmd = "devin --model swe-2-medium"  # worker 的 agent 命令行
# integration = "branch"     # 覆盖默认集成分支 issue-<spec>-integration
# require_review = true      # worker 提示词是否强制 leader + 第三方双审（默认 true）
# hc_socket = "/run/user/1000/hc.sock"  # herdr-core socket（与 hc_binary 二选一）
# hc_binary = "hc"                      # 或经 hc connect 连接
```

要点：

- **`agent_cmd`** 按简化 shell 规则拆分（空格分词、单双引号、反斜杠
  转义）。未配置时退化为 `devin [--model <devin_model>]`。
- **`hc_binary` 与 `hc_socket` 互斥**：前者执行 `hc connect` 拉起/连接
  herdr-core，后者直连已有 Unix socket。
- **`retries = 0` 是合法值**（只尝试一次，不续写）。

### 提示词模板 fastimpl.prompt.md

查找顺序与配置相同：`<work-dir>/fastimpl.prompt.md` > `<exe 目录>`，缺失时
把内置默认模板播种到 exe 目录。

模板按 Go `text/template` 渲染，可用 `{{if}}`/`{{range}}` 等全部动作
语法。可用变量：

- `{{.IssueNo}}` — 子任务 issue IID
- `{{.SpecNo}}` — spec issue IID
- `{{.Token}}` — 本任务的随机报告令牌
- `{{.WorkDir}}` — fastimpl 运行目录，也是 agent 的启动目录（cwd）；
  其中含主仓库 checkout 及可能的其他目录
- `{{.RepoDir}}` — 目标仓库主 checkout 的绝对路径（供参考，不修改）
- `{{.Worktree}}` — fastimpl 为本任务创建的 git worktree 绝对路径，
  agent 的代码产出目录（修改与提交只在这里进行）
- `{{.IntBranch}}` — 集成分支名：fastimpl 把各任务分支合并于此，
  冲突时 agent 需知道跟谁比较
- `{{.Branch}}` — 本任务的工作分支名：agent 的提交落在它上面，
  最终由 fastimpl 合入集成分支
- `{{.Forge}}` — 上游 forge 类型（`gitlab`/`github`）
- `{{.ForgeCLI}}` — 对应的 issue 命令行工具（`glab`/`gh`）
- `{{.RequireReview}}` — 是否强制 leader + 第三方独立审核
  （`require_review` 配置，默认 true）

模板与 fastimpl 的**唯一契约是报告协议**：任务全部完成（含仓库要求的
验证）后在回复末尾单独一行输出 `FASTIMPL-READY <token>`，随后最多六行
说明实现/验证/阻塞情况。定制模板时**不要改动这行的格式**；未完成时
不要输出该标记。

### 目录解析约定

```
work-dir   显式 > 当前目录
repo-dir   -repo-dir > 配置 repo_dir > 从 work-dir 向上/向下各搜 8 级，
           唯一命中即用；零个/多个都报错
tree-dir   -tree-dir > 配置 tree_dir > 既有 worktree 的统一父目录
           （不统一报错）> <repo-dir>/.worktrees
forge      配置 forge > origin 主机名推断 > glab/gh 探测 > gitlab
```

仓库根判定是目录下存在 `.git` **目录**（linked worktree/submodule 的
`.git` 文件不算主仓库根）。

### 观察与排查

- **实时查看某个 agent**：启动日志里有
  `worker running key=<proj!iid> terminal=<repo>-issue-<iid> addr=…`，
  直接 `hc attach <repo>-issue-<iid>` 进终端看输出、必要时接管对话。
- **查看整体进度**：`-print` 随时可跑（只读）；MR/PR 页面上能看到集成
  分支累积的提交。
- **needs-attention 任务**：日志有
  `task needs attention key=… reason=…`，issue 上有说明评论，hc 终端
  和 worktree 都保留——`hc attach` 进去看 agent 到底输出了什么，或直接
  在 worktree 里检查代码。
- **集成终端**：base 冲突由 `<repo>-issue-<spec>-integration` 终端处理；
  未解决时同样保留并打印 attach 提示。
- **常见失败原因**：agent 未输出标记（提示词/模型行为问题，看终端）、
  轮次超时（90min，复杂任务可拆小）、claim 被拒（权限）、跨项目前置
  仍开放（只能等外部关闭或人工处理后重跑）。

### 中断与重跑

- **Ctrl+C / SIGTERM**：`signal.NotifyContext` 取消运行，调度器走 defer
  清理（回收集成终端等）后退出；正在跑的任务终端与 worktree 保留。
- **重跑**：命令是幂等的——已关闭/已集成的不会重做；已存在的
  `issue-*` 分支与 worktree 会复用；集成分支存在则在其上继续。把上次
  needs-attention 的 issue 处理好（或确认它本就该被关闭）后直接再次
  `fastimpl <spec>` 即可续跑。

## 安全约定

- 不把仓库内容发给任何外部"判定"服务：完成与否只看 agent 在终端里
  输出的 `FASTIMPL-READY <token>`。
- `FASTIMPL_LLM_API_KEY`、`FASTIMPL_LLM_BASE_URL`、`FASTIMPL_LLM_MODEL`
  三个环境变量**不会传给任何子进程**：配置加载时从进程环境清除，
  glab/gh/worker 终端的子进程环境单独做 scrub。
- claim 前重读 issue 状态，已被他人领取的任务不抢占；跨项目 issue
  永不被修改（不 claim、不关闭、不评论）。

## 项目结构

```
cmd/fastimpl        命令行入口：flag 解析、signal.NotifyContext
cmd/fastimpl-gui    GUI 版占位（规划：每个 hc 会话一个 nsgo tab）
app                 运行管线编排（CLI/GUI 共用）
config              fastimpl.toml 分层加载、校验、agent_cmd 拆分、env 清理
files               文件查找约定：work-dir > exe 目录（config/prompt 共用）
worktree            git 操作：FindRoot/ResolveTreeDir/分支与 worktree CRUD/
                    merge/push/SyncBase/ConflictError
gitlab / github     forge 客户端：依赖图源（Spec/Children/BlockedBy）+
                    forge.Forge 实现（IssueState/Claim/Close/Note/EnsureMR/Probe）
forge               Forge 接口与 IssueInfo：issue/MR 操作抽象（调度器↔客户端共享）
graph               依赖图：Build 传递闭包、Ready/Done、拓扑分层、文本/JSON 渲染
prompt              fastimpl.prompt.md 加载/播种与 {issue-no}/{token} 渲染
worker              hcgo 终端管理：启动/发提示词/轮询 7501/读屏/停止；
                    FASTIMPL-READY 报告协议与令牌
scheduler           调度核心：就绪即派、任务生命周期、串行集成、
                    冲突交 agent、base 同步、状态刷新
```

## 开发与测试

```bash
go build ./...
go vet ./...
go test ./...
```

测试亮点：`scheduler` 的用例用**真实 git 仓库**跑端到端场景——依赖顺序
调度、外部关闭跳过、任务↔集成冲突由任务 agent 解决、base↔集成冲突由
集成专属 agent 解决；`config`/`graph`/`worker`/`worktree` 各有针对目录
解析、图算法、报告协议、分支匹配的单测。
