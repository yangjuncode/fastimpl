# Task Plan: GUI 保存配置后本次调度仍使用旧配置

## Goal
确保 GUI 设置页保存后的配置与提示词，在用户点击“开始调度”真正开工时使用最新磁盘内容，并用回归测试锁住该行为。

## Next Step
提交 work_report 并向用户总结。

## Current Phase
Phase 5

## Phases

### Phase 1: Requirements & Discovery
- [x] 理解用户症状：GUI 设置保存后，开始工作仍用旧配置
- [x] 梳理配置分层、GUI 保存、app 运行管线与调度门禁
- [x] 记录发现到 findings.md
- **Status:** complete

### Phase 2: Reproduction & Diagnosis
- [x] 建立能捕获“门禁等待期间保存的新配置未生效”的回归测试
- [x] 确认提示词加载时机与配置加载时机差异
- **Status:** complete

### Phase 3: Implementation
- [x] 在调度门禁通过后、真正准备集成/连接 hc/启动 worker 前重新加载配置
- [x] 为后续尚未启动 worker 与续写重试热加载安全配置/提示词
- [x] 保持命令行/UI 显式覆盖优先级与既有 log_file 语义
- **Status:** complete

### Phase 4: Testing & Verification
- [x] 运行相关单元测试
- [x] 运行全量 Go 测试与必要静态检查
- **Status:** complete

### Phase 5: Delivery
- [ ] 总结根因、修改与验证结果
- [ ] 通过 Agent-Assistant work_report 汇报
- **Status:** pending

## Decisions Made
| Decision | Rationale |
|----------|-----------|
| 优先修复 app 层门禁后的配置刷新 | GUI/CLI 共用管线，真正 stale 状态在 app.run 内形成 |

## Errors Encountered
| Error | Resolution |
|-------|------------|
| 初次初始化 planning 文件在根目录而 .active_plan 指向旧任务 | 本次按根目录新计划继续，避免覆盖旧命名计划目录 |
