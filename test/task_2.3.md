# Go 包测试报告 task_2.3

## 第 1 次测试

### 判定：FAIL

| # | 测试维度 | 位置 | 失败原因 | 修复建议 |
|---|----------|------|----------|----------|
| 1 | 并发安全 | `concurrent_test.go:L189-190` | 并发测试中活跃数统计出现竞态条件，10 个 goroutine 竞争 3 个许可时，检测到 active count 达到 4 超过限制 3 | 在 `GetActiveCount()` 中增加 RLock 保护，或在 `ConcurrentAccess` 测试中增加同步等待确保统计准确性 |
| 2 | 并发安全 | `orchestrator.go:L133-138` | `NewOrchestrator` 中并发控制器初始化使用了硬编码的默认值（MaxGlobal=5, MaxPerProject=2），但未从配置中读取 MaxPerProject，导致项目级并发限制无法配置化 | 从 `config.HarnessConfig` 扩展配置项 `MaxPerProject` 并传入 `ConcurrentControllerConfig` |
| 3 | 功能完整性 | `orchestrator.go:L193-209` | 并发控制获取许可后缺少 `defer` 保护，若 `executeWithStateProtection` 发生 panic，许可将泄漏无法释放 | 将 `Acquire` 后添加 `defer o.concurrentCtrl.Release(o.projectName)`，移除手动 `needRelease` 标志 |
| 4 | 资源隔离 | `concurrent.go:L168-183` | `getProjectPool` 中项目池创建缺少锁保护，多个 goroutine 同时首次访问相同 projectName 时可能创建重复的项目池通道，导致并发限制失效 | 使用 `sync.Once` 或 `map[string]*sync.Once` 确保每个项目池只创建一次，或在 `getProjectPool` 外持有写锁完成整个 `Acquire` 流程 |
| 5 | 测试覆盖 | `concurrent_test.go` | 缺少边界测试：空字符串 projectName、极值 MaxGlobal=0/1、大量项目名（100+）下的内存泄漏测试 | 补充空项目名、零值配置、100+ 项目名循环创建/清理的测试用例 |

## 第 2 次测试（重测）

### 判定：FAIL

| # | 上次问题 | 当前状态 |
|---|---------|----------|
| 1 | 并发读写统计竞态 | ⚠️ 部分修复（`GetActiveCount()` 已加锁，但 `TestConcurrentController_ConcurrentAccess` 仍间歇性失败，10 次运行失败 4 次，active count 仍偶发达到 4） |
| 2 | MaxPerProject 配置缺失 | ✅ 已修复（`config.HarnessConfig` 已添加 `MaxPerProject` 字段，`orchestrator.go` 已读取配置） |
| 3 | defer 保护缺失 | ✅ 已修复（`orchestrator.go:L212-214` 已添加 defer 保护） |
| 4 | 项目池创建竞态 | ⚠️ 部分修复（`acquireProject` 中锁保护了项目池创建，但锁在创建后释放，select 获取许可在锁外，仍存在理论上的竞态窗口） |
| 5 | 边界测试缺失 | ✅ 已修复（已添加 `TestConcurrentController_EmptyProjectName`, `TestConcurrentController_ExtremeValues`, `TestConcurrentController_ManyProjectsMemory`） |

## 第 3 次测试（重测）

### 判定：PASS