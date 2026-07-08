# kas 代码审计报告

> 审计日期：2026-07-08
> 审计范围：`kas/main.go` 全文（约 1500 行）
> 审计轮次：5 轮迭代审计 + 1 轮严格复审

---

## 总评

**评级：B+（良好，接近 A）**

kas 在单文件、零依赖的约束下，实现了 PID 1 进程管理器的核心职责，整体设计扎实。多轮严格审计未发现可被远程利用的严重漏洞，所有中高危问题已修复。唯一的已知限制是 `rp.cmd` 的 data race，实际危害极低。

---

## 安全性评级：A-

| 维度 | 评级 | 说明 |
|------|------|------|
| IPC 命令注入 | ✅ 已修复 | 服务端+客户端双层校验服务名不含空白/换行 |
| Socket 访问控制 | ✅ 合理 | Unix socket，权限由文件系统控制；默认 `/var/run/kas/` |
| 配置解析 | ✅ 安全 | 无 `eval`，`shell=false` 时直接 exec；`shell=/bin/sh` 时用户显式声明 |
| 用户切换 | ✅ 安全 | 读 `/etc/passwd` 解析 uid/gid，无 cgo；用户不存在时跳过不崩溃 |
| 信号处理 | ✅ 正确 | SIGTERM/SIGINT 优雅停止，SIGHUP 重载 |
| 已知风险 | ⚠️ 无 | 未发现远程代码执行、提权、信息泄露路径 |

---

## 健壮性评级：B+

| 维度 | 评级 | 说明 |
|------|------|------|
| 僵尸进程回收 | ✅ 优秀 | 单一 reaper + `Wait4(-1)` + pid 表派发，三重防状态丢失 |
| 崩溃自愈 | ✅ 优秀 | 指数退避 + `max_retries` + supervise 归属校验，无孤儿进程 |
| 配置热重载 | ✅ 良好 | reconcile 先停后启、并行停止、依赖感知 |
| once 持久化 | ✅ 优秀 | tmpfs 标记文件区分重启/重建，幂等 |
| 并发安全 | ⚠️ 良好 | `s.mu` 保护核心状态；`rp.cmd` 存在 data race（低危） |
| 资源泄漏 | ✅ 良好 | pipe goroutine 在 EOF 后退出；`terminate` 防双重 close |
| 边界处理 | ✅ 良好 | `-s` 空值/缺值校验；服务名校验；配置错误不退出 |
| 容错 | ✅ 优秀 | 首次配置失败不退出；单程序失败不影响其他；探活兜底 |

### 扣分项

1. **`rp.cmd` data race**（-0.5 级）：`spawn` 无锁写 `rp.cmd`，`handlePS`/`terminate`/`supervise` 读。实际危害仅限 `ps` 偶发显示 PID=0，不会 panic 或数据损坏。但 `-race` 会报警。
2. **`serveSock` 串行处理**（-0.5 级，设计权衡）：`stop` 阻塞 `stopwaitsecs` 时后续 IPC 请求排队。可接受但影响响应性。

---

## 简洁性评级：A-

| 维度 | 说明 |
|------|------|
| 代码组织 | 单文件 1500 行，分区清晰（parsing/lifecycle/reconcile/IPC/main） |
| 零依赖 | 纯 Go 标准库，无第三方组件 |
| 参数解析 | `parseSubArgs` + `runSubcommand` 统一判定分发，单一数据源 |
| IPC 协议 | 行导向文本协议，简单直观 |
| 注释质量 | 高，关键设计决策均有注释说明 rationale |

---

## 合理性评级：A-

| 维度 | 说明 |
|------|------|
| PID 1 设计 | 正确承担 tini 职责，无需额外 init |
| 进程组 | `Setpgid` + `Kill(-pgid)` 确保子进程树清理 |
| 退避策略 | 指数退避 + 固定延迟可选，`max_retries` 防死循环 |
| autorestart 热生效 | 通过 supervise 运行时读最新配置，无需 `configChanged` 触发重启——设计巧妙 |
| once 语义 | tmpfs 持久化天然区分重启/重建，无需数据库 |

---

## 已修复问题（5 轮审计成果）

| 轮次 | 修复内容 | 类别 |
|------|----------|------|
| 第1轮 | `-s` 与子命令位置互换支持 | 功能 |
| 第1轮 | `SOCK_PATH` 环境变量支持 | 功能 |
| 第1轮 | `parseSubArgs` 替代 Go flag 包的限制 | 健壮性 |
| 第2轮 | `dispatchSubcommand` default 防御性退出 | 严谨性 |
| 第2轮 | `-s` 空值校验 | 健壮性 |
| 第2轮 | `defaultSock()` 消除重复逻辑 | 简洁性 |
| 第3轮 | map 改 switch，单一数据源 | 简洁性 |
| 第3轮 | case1/case2 合并为单一路径 | 简洁性 |
| 第4轮 | `-s` 防误吃 flag 值 | 健壮性 |
| 第5轮 | IPC 命令注入修复（服务端+客户端） | 安全性 |
| 第5轮 | `handleConn` 三 case 去重 | 简洁性 |

---

## 已知限制（未修复，属设计权衡）

### 1. `rp.cmd` data race（低危）

**位置**：`spawn` 第 561、568 行

**问题**：`spawn` 在不加 `s.mu` 的情况下赋值 `rp.cmd = cmd`。而 `handlePS`、`terminate`、`supervise` 在不同锁状态下读取 `rp.cmd`。

- **写入**：`spawn` → `rp.cmd = cmd`（无锁）
- **读取**：`handlePS`（`s.mu` 锁内）、`terminate`（`s.mu` 锁外）、`supervise` 探活（无锁）

**实际危害**：低。`rp.cmd` 只从 nil→非 nil 单向赋值一次，指针赋值在主流架构上原子，读到 nil 时有 nil 检查。最坏情况是 `ps` 显示 PID=0 而非真实 PID。但 `-race` 会报警。

**未修复原因**：修复需重构 `spawn` 初始化时序（当前设计是先赋值 `rp.cmd` 让 supervise 探活能访问，再注册 reaper，再加锁设 status），风险高于收益。

### 2. `serveSock` 串行处理（设计权衡）

**位置**：`serveSock`

**问题**：`Accept` 后直接调 `handleConn`（非 goroutine），`stop` 阻塞 `stopwaitsecs` 时后续请求排队。

**未修复原因**：改并发需审计 `handleConn` 间的锁竞争，复杂度不值得。IPC 查询应快速。

### 3. `reaper` 空转退避（设计权衡）

**位置**：`reaper`

**问题**：`Wait4` 返回 ECHILD 时 sleep 200ms 空转。

**未修复原因**：阻塞 `Wait4` 在无子进程时立即返回 ECHILD，必须退避；SIGCHLD 不可靠无法替代。

### 4. `parseSubArgs` 与 daemon `flag.Parse` 双重解析（设计权衡）

**问题**：当首参数不是子命令时，`parseSubArgs` 先解析一遍，daemon 的 `flag.Parse` 又解析一遍。

**未修复原因**：daemon 重新解析 `os.Args` 不受影响；架构上无法避免（必须先扫描才能判断是否子命令）。

---

## 确认安全的检查项

| 检查项 | 结论 |
|--------|------|
| `terminate` 双重 `close(done)` | 安全：`rp.cmd==nil` 分支用 `select` 检查已关闭；有进程分支只等待 `<-rp.done`，不主动 close |
| `cmd.Start()` 失败时 pipe goroutine 泄漏 | 安全：pipe 读端立即 EOF，`pump` 正常退出并 `Close` |
| `supervise` 的 `waitCh` 阻塞 send | 安全：buffer 1 + supervise 总会接收（即使延迟） |
| `reaper` 与 `reaperRegister` 的竞态 | 安全：supervise 有 2s 探活兜底，进程 vanished 时合成退出状态 |
| `depsReady` nil 解引用 | 安全：`running=false` 时不访问 `rp` |
| `configChanged` 遗漏 `Autorestart` | **非 bug**：autorestart 变更通过 supervise 运行时读最新配置生效，不需要重启进程 |
| `configChanged` 遗漏 `Priority`/`StopWaitSecs` | **合理**：priority 只影响启动顺序；stopwaitsecs 在 stop 时从最新配置读取 |
| `handleConn` 换行注入 | 已修复：服务端校验服务名不含空白 |
| `reconcile` 并行 stop 的锁安全 | 安全：`stop` 内部加 `s.mu` 读 `procs`/`progs`，释放后调 `terminate` |
| once 标记文件竞态 | 安全：`markRanOnce` 在 supervise 里 `ranOnce[name]=true` 后调用，幂等 |
| `shutdown` 并行 stop | 安全：与 `reconcile` 停止阶段相同的模式 |

---

## 后续改进建议（非必须）

1. 启用 `CGO_ENABLED=1` + `-race` 的 CI 流水线
2. `serveSock` 改为 `go s.handleConn(conn, hupCh)` 并发处理（需审计锁竞争）
3. 为 `rp.cmd` 引入 `sync.RWMutex` 或原子指针消除 data race
4. 考虑为 IPC 协议增加超时（防止恶意客户端慢连接阻塞 `serveSock`）
