# kas

一个面向 Docker 容器的轻量级 PID 1 多进程管理工具，**tini + 进程管理器**的结合体。

- **PID 1 职责**：单一 reaper goroutine 统一回收所有子进程（被管理进程 + reparent 的孤儿），避免僵尸堆积。**无需 tini**，kas 自身即可作为容器入口
- **多进程管理**：读取 ini 配置文件，按 `[program:NAME]` 段管理多个进程（`program:` 是固定前缀，`NAME` 是服务名）
- **手动重载**：通过 `kas reload` 或 `SIGHUP` 触发配置重载（无自动轮询，按需重读）
- **运行时控制**：通过 `autostart` 指令在不重启 kas 的情况下动态启停单个程序（reload 生效）
- **崩溃自愈**：`autorestart=true` 的程序崩溃后按指数退避或固定间隔自动重启，`max_retries` 防止死循环
- **初始化任务**：`type=once` 的一次性任务仅在容器创建时执行一次，重启不重跑
- **启动依赖**：`depends=a,b` 声明依赖关系，被依赖项就绪后才启动
- **状态查询**：`kas ps` 通过 unix socket IPC 查看所有进程实时状态
- **零依赖**：纯 Go 标准库实现，单文件，无第三方组件

## 编译

```bash
cd kas
go build -o kas .
```

## 运行

```bash
# 默认配置文件 /etc/kas.ini，默认 socket /var/run/kas/kas.sock
./kas

# 指定配置文件和 socket
./kas -c /path/to/kas.ini -s /path/to/kas.sock

# 查看帮助
./kas -h
```

作为容器入口运行：

```dockerfile
ENTRYPOINT ["/usr/local/bin/kas"]
# 或指定配置：ENTRYPOINT ["/usr/local/bin/kas", "-c", "/etc/kas.ini"]
```

> **不需要 `tini -- kas`**：kas 内置僵尸进程回收，直接作为 PID 1 即可。

### 子命令

```bash
# 查看所有被管理的进程（连接运行中的 kas）
./kas ps
./kas ps -s /var/run/kas/kas.sock

# 触发配置重载
./kas reload
./kas reload -s /var/run/kas/kas.sock

# 启动指定服务（即使 autostart=false 也能手动启动）
./kas start web
./kas start web -s /var/run/kas/kas.sock

# 停止指定服务
./kas stop web
./kas stop web -s /var/run/kas/kas.sock

# 重启指定服务
./kas restart web
./kas restart web -s /var/run/kas/kas.sock
```

`start`/`stop`/`restart` 通过 IPC 对运行中的 kas 发出指令。once 类型且已 initialized 的服务不可 start/restart。

`ps` 输出示例：

```
NAME  PID      TYPE  STATUS       RESTARTS  UPTIME
init  0        once  initialized  0
web   12345    long  running      2         5m30s
off   0        long  stopped      0
```

| 列 | 说明 |
|------|------|
| NAME | 服务名（`[program:NAME]` 的 NAME） |
| PID | 进程 PID（未运行时为 0） |
| TYPE | `once` 或 `long` |
| STATUS | `starting` / `running` / `stopped` / `initialized` |
| RESTARTS | 自动重启次数 |
| UPTIME | 已运行时间（仅 running 状态） |

## 配置文件

配置文件采用 ini 格式，每个 `[program:NAME]` 段定义一个被管理的程序。`program:` 是固定前缀，`NAME` 是服务名（用于 `depends=`、日志、`kas ps`）。

### 完整字段

| 字段 | 说明 | 默认值 |
|------|------|--------|
| `command` | 要执行的命令（经 `/bin/sh -c` 执行） | 必填 |
| `autostart` | kas 启动时是否自动拉起；reload 时改为 `false` 停止该服务 | `true` |
| `autorestart` | 进程退出后是否自动重启 | `true` |
| `stopwaitsecs` | 停止时 SIGTERM 后等待多少秒再 SIGKILL | `10` |
| `user` | 以哪个用户身份运行（从 `/etc/passwd` 解析，仅 Linux） | 继承 kas |
| `environment` | 环境变量，`KEY="val",KEY2="val2"` 格式 | 无 |
| `stdout_logfile` | 标准输出日志文件 | 控制台 |
| `stderr_logfile` | 标准错误日志文件 | 控制台 |
| `priority` | 启动优先级，数值小的先启动 | `999` |
| `type` | **kas 扩展**：`once`（一次性初始化任务）或 `long`（长服务） | `long` |
| `depends` | **kas 扩展**：逗号分隔的依赖服务名，需先启动/完成 | 无 |
| `max_retries` | **kas 扩展**：崩溃后最大重试次数，`0` 表示无限重试 | `3` |
| `restart_delay` | **kas 扩展**：重启固定间隔秒数，`0` 表示指数退避（0.5s→30s） | `0` |

### `${VAR}` 环境变量展开

所有字段值都支持 `${VAR}` 形式的环境变量展开（取自 kas 进程环境）：

```ini
command=/app --port ${APP_PORT}
environment=LOG_DIR="${LOG_ROOT}/app"
stdout_logfile=${LOG_ROOT}/web.log
```

### `autostart` 运行时控制

`autostart` 同时是运行时控制开关：在配置中把 `autostart` 改为 `false`，执行 `kas reload` 即停止该服务；改回 `true` 再 reload 则重新启动。无需重启 kas。

### `type` 指令（任务类型）

- `type=long`（默认）：长运行服务，崩溃后按 `autorestart` + 退避策略重启
- `type=once`：一次性初始化任务，仅在容器创建后首次加载配置时启动一次，执行完即结束，**不重启**、reload 时也不重复启动

`once` 任务的 `autorestart` 会被强制设为 `false`。

**once 的持久化**：once 任务执行后（无论成功失败）会在 `/var/run/kas/once.done/<NAME>` 创建标记文件。`/var/run` 是 tmpfs：
- **容器重启**（`docker restart`）：tmpfs 保留，标记文件还在 → once **不重跑**
- **容器重建**（`docker run` / `docker compose up` 重新创建）：tmpfs 清空 → once **重跑**

### `depends` 指令（启动依赖）

逗号分隔的服务名列表，本程序需等依赖就绪后才启动：

- 依赖是 `long` 类型：依赖进入运行状态即视为就绪
- 依赖是 `once` 类型：依赖执行完成才视为就绪

`once` 依赖完成后会自动触发一次 reconcile，让等待它的程序启动。依赖未就绪时本程序跳过本次启动，等下次 reload 或依赖完成时再尝试。

### 重启控制（`max_retries` / `restart_delay`）

防止崩溃程序无限重启导致死循环：

- `max_retries=N`：崩溃后最多重试 N 次。达到上限后程序停止重启，进入"放弃"状态，直到配置 reload（重置计数）。`0` 表示无限重试（不推荐）
- `restart_delay=N`：每次重启的固定间隔秒数。`0`（默认）表示指数退避（500ms→1s→2s→...→30s 封顶）

```ini
[program:web]
command=/app
autorestart=true
max_retries=5
restart_delay=2
```

### 日志输出

- **未指定 `stdout_logfile`/`stderr_logfile`**：子进程 stdout 转发到 kas 的 stdout，stderr 转发到 kas 的 stderr，带 `[name:stream]` 前缀
- **指定了日志文件**：仅写入文件，不镜像到控制台
- `AUTO`/`NONE` 与未指定等价，走控制台

### 配置示例

```ini
; 一次性初始化任务：仅容器创建时执行，web 依赖它完成
[program:init]
command=/usr/local/bin/init-script
autostart=true
autorestart=false
type=once
priority=1
stdout_logfile=/tmp/init.log

[program:web]
command=/usr/local/bin/myapp --port ${APP_PORT}
autostart=true
autorestart=true
stopwaitsecs=10
max_retries=5
restart_delay=2
priority=10
type=long
depends=init
stdout_logfile=/tmp/web.log
stderr_logfile=/tmp/web.err

[program:worker]
command=/usr/local/bin/worker
autostart=true
autorestart=true
priority=20
user=nobody
environment=LOG_LEVEL="debug",QUEUE="default"

; 配置在案但暂时不启动的服务（reload 改 autostart=true 即可启动）
[program:maintenance]
command=/usr/local/bin/maint-task
autostart=false
autorestart=false
```

完整示例见 `kas.ini.example`。

## 信号行为

| 信号 | 行为 |
|------|------|
| `SIGTERM` | 对所有被管理进程的进程组发 SIGTERM（超 `stopwaitsecs` 再 SIGKILL），全部退出后 kas 退出 |
| `SIGINT` | 同 `SIGTERM` |
| `SIGHUP` | 触发配置重载（等价于 `kas reload`） |

## 指定用户执行

`user=NAME` 让程序以该用户身份运行：

- kas 读 `/etc/passwd` 解析 uid/gid（纯 Go，无 cgo 依赖）
- 通过 `syscall.Credential` 切换身份
- 仅 Linux 可用，其它平台打日志告警并跳过
- 用户不存在时打日志，不影响其它程序

> 注意：kas 自身需以 root 运行才能切换到任意用户；以普通用户运行时只能切到同 uid 的用户。

## IPC 机制

kas 启动时监听 unix socket（默认 `/var/run/kas/kas.sock`，可用 `-s` 覆盖），`kas ps` 和 `kas reload` 作为独立进程通过该 socket 与运行中的 kas 通信：

- `ps`：kas 返回所有程序的实时状态快照
- `reload`：kas 触发一次配置重载

socket 文件在 kas 启动时清理旧文件、shutdown 时删除。

## 设计要点

- **统一 reaper**：单一 goroutine 用阻塞 `Wait4(-1)` 回收所有子进程（被管理进程 + reparent 的孤儿），通过 pid→runningProc 表把退出状态派发给对应 supervise goroutine，避免 wait 争抢导致状态丢失
- **进程组**：每个程序用 `Setpgid` 独立成组，停止时操作整个组，确保子进程一并终止
- **配置同步**：重载时按 `priority` 升序处理；配置变更（command/user/env/logfile）的程序先停后启；从配置中移除的程序会被停止并清理；停止阶段并行执行避免慢程序拖慢整体
- **退避重启**：崩溃程序按指数退避或固定间隔重启，通过指针比较校验 supervise 归属，避免与 reload 的竞态产生孤儿进程；`max_retries` 防止死循环
- **once 持久化**：标记文件存于 `/var/run`（tmpfs），天然区分容器重启（不重跑）与重建（重跑）
- **容错**：首次配置加载失败只打日志不退出；单个程序启动失败不影响其它程序；reaper 与 supervise 间有探活兜底防止极端 race 下状态丢失
