# Progress

## 2026-11-27

- 初始化本次调试计划。
- 阅读 `config/config.go`、`prompt/prompt.go`、`cmd/fastimpl-gui/settings.go`、`cmd/fastimpl-gui/session.go`、`app/app.go`。
- 初步定位：配置在构图前加载，GUI 门禁等待期间保存的新配置不会刷新到本次运行；提示词在门禁后加载，时机较新。
- 已建立红色回归测试 `TestRunObservedReloadsConfigAfterScheduleGate`：在 `BeforeSchedule` 中把配置改为非法值，当前代码仍使用门禁前旧配置并继续走到缺失模板错误，证实 stale Config。
- 用户通过 Agent-Assistant 确认：修改发生在点击“开始调度”前；未使用 -config/-template；期望后续尚未启动 worker 与续写重试也使用最新配置/提示词。
- 已实现并验证门禁后配置重载：`TestRunObservedReloadsConfigAfterScheduleGate` 通过。
- 已实现 scheduler 热更新快照：后续新 worker 的 agent 命令/首次 prompt/reviewer、续写重试的 require_review、并发与 retries、terminal_retention 都会重读。
- GUI 保存的 fastimpl.toml / fastimpl.prompt.md 改为原子写，避免热加载读到半份文件。
- `go test -race ./app ./scheduler` 通过。
- 已补 `TestRuntimeSettingsLoaderFallsBackToLastGoodSnapshot` 与 `files.WriteFileAtomic` 权限/内容回归测试。
- 最终验证通过：`go test ./...`、`go test -race ./app ./scheduler`、`go vet ./...`、`git diff --check`。
- 注意：工作区还存在 `graph/graph.go` 与 `cmd/fastimpl-gui/graph_test.go` 的非本修复改动（集成后同步 Closed 状态），未回退、未纳入本修复范围。
