# Findings

## 配置保存与读取链路

- GUI 设置页 `saveSettings` 会把表单配置写入 `<work-dir>/fastimpl.toml`，把提示词写入 `<work-dir>/fastimpl.prompt.md`。
- `config.Load` 的优先级是内置默认值 → exe 同目录配置 → work-dir 配置或显式 `-config`。
- `app.run` 当前在建依赖图之前执行一次 `config.Load`；随后 `observer.BeforeSchedule` 会让 GUI 停在“开始调度”门禁前。
- 门禁返回后，当前代码才执行 `prompt.Load`，因此提示词文本本身通常是新的；但 `cfg` 仍是构图前加载的旧副本，后续的 `tree_dir`、`integration`、`agent_cmd`、`hc_*`、`parallelism`、`retries`、`require_review`、`reviewer`、终端保留策略等都使用旧值。
- 结论假设：用户看到的“保存了但开工仍用旧配置”主要来自 GUI 等待开始期间配置已被保存到磁盘，却没有在门禁后重新加载。

## 需要注意的语义

- `repo_dir` 与 `forge` 已在构图前使用；如果用户在门禁等待期间改它们，仅重载配置不能让已建好的图 retroactively 变成新仓库/新 forge 的图。
- `log_file` 文档语义是下一次启动运行时生效；本次修复不应让运行中突然切换日志文件。
- 命令行 `-j` 与 UI/命令行显式目录覆盖仍应优先于配置文件。

## 用户确认的范围

- 复现时机：还没点击“开始调度”时先修改并保存，然后点击开始。
- 启动参数：没有显式使用 `-config` 或 `-template`。
- 期望范围：不仅开工前生效；即使调度已开始，之后尚未启动的 worker 与续写重试也应尽量使用最新配置和提示词。

## 热更新字段分层

适合在轮次内热更新的字段：`parallelism`、`retries`、`agent_cmd`、`require_review`、`reviewer`、`terminal_retention` 与提示词模板。它们影响之后新启动的 worker、首次 prompt、续写重试或收尾策略。

不宜在轮次内热切换的字段：`repo_dir`、`tree_dir`、`forge`、`integration`、`hc_binary`、`hc_socket`、`instance_id`、已有终端的 agent 进程。它们已经在构图、集成分支、hc 连接或进程启动时消费，热切换会破坏身份/恢复一致性。

## 最终结论（2026-11-27）
- 根因不是 GUI 保存失败，而是读取时机：配置在构图前加载进内存，GUI 可能在 BeforeSchedule 门禁前等待很久；门禁后原实现只重新读提示词，不重读配置。
- 修复分两层：门禁放行后、集成分支/hc/scheduler 初始化前重读一次完整运行配置；scheduler 运行中再通过 RuntimeSettingsLoader 在派发/首次 prompt/续写重试/终端收尾边界重读可安全热更新字段。
- 可热更新字段限定为 parallelism、retries、agent_cmd、require_review、reviewer、terminal_retention 与提示词模板；repo/forge/hc/integration/instance 等已在构图、连接或恢复身份中消费的字段保持本轮固定。
- GUI 保存 fastimpl.toml 与 fastimpl.prompt.md 改为同目录临时文件 + rename 的原子替换，热加载读者不会看到半份文件。
- 运行中读到暂时非法/不可读配置时，scheduler 记录 warning 并回退到最后一次成功快照，避免一次手写坏配置打爆所有在跑任务。
