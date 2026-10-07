# kvs — 反向代理网关

kai vscode

轻量级 Go 反向代理。提供 **Cookie 认证**、**多后端路由（前缀/正则）**、**服务自动部署**、**外部资源代理（带缓存）**、**退出按钮 + Update 菜单注入（VS Code专有）**、**S3 镜像同步（VS Code专有）**。仅依赖 Go 标准库 + `embed`，无第三方依赖，**完全通过 `kvs.ini` 配置文件驱动**。

## 快速开始

```bash
make build
./kvs help                          # 查看帮助
./kvs demo                          # 生成示例配置 ./kvs.ini（内置唯一模板）
# 编辑 kvs.ini —— 各键说明见模板内注释
./kvs -c kvs.ini                    # 指定配置文件启动（默认用文件里第一张路由表）
./kvs -c kvs.ini -n :vscode         # -n :名字 选择 [proxies.vscode] 路由表
./kvs                                # 缺省使用内嵌 kvs.ini（第一张表 = zcodex 应用中心，见 agents.md）
./kvs -c kvs.ini                     # 使用指定配置文件
./kvs -n "/=http://127.0.0.1:8080"  # 内联路由（无 [service.<name>] 段，svc:// 不可用）
```

**`-c` 可选，只接受文件路径**：不带 `-c` 时使用内嵌 `kvs.ini`；带 `-c` 而文件不存在直接报错退出（预设别名已全部移除）。配置内可以定义多张命名路由表 `[proxies.<name>]`，用 `-n :<name>` 选择其一，未选择时用文件中**第一张表**。`-n "prefix=url;…"` 为内联路由（内联模式没有 `[service.<name>]` 段，`svc://` 目标不可用）；环境变量 `KVS_PROXIES` 与内联 `-n` 等价。zcodex 应用中心（`api://manager`）的页面功能与内部机制见 [agents.md](agents.md)。

## 子命令

| 命令 | 说明 |
|---|---|
| `kvs help` | 显示帮助信息（也支持 `-h`、`--help`） |
| `kvs demo [default\|vscode\|zcodex]` | 生成示例配置文件 `kvs.ini`（单模板，选择路由表用 `-n :<name>`） |
| `kvs mirror [vscode] [-version v] [-platform p]` | 同步 VS Code 版本到 S3 兼容存储；`-version` 默认 latest，存储配置只走 `KVS_MIRROR_S3_PREFIX/_ACCESS/_SECRET/_REGION` 环境变量（不读配置文件） |
| `kvs syncto <src> <dst>` | 本地 ↔ S3 文件同步（见下方说明） |

`kvs syncto` **只读环境变量，不读 kvs.ini**，缺少必需变量直接报错：`KVS_S3_PREFIX`（S3 兼容服务基础 URL，bucket 根）、`KVS_S3_ACCESS`、`KVS_S3_SECRET` 必需，`KVS_S3_REGION` 可选（默认 us-east-1）。

```bash
kvs syncto /zcode s3:/vsc/zcode   # 上传：本地文件/目录 → S3（递归，同名覆盖）
kvs syncto s3:/vsc/zcode /zcode   # 下载：S3 → 本地（对象或前缀递归）
```

- `s3:` 与本地路径二选一（`s3:/bucket内的key路径`）；不支持两个 S3 对拷
- 目录按相对路径 1:1 映射为对象 key；单文件时 `s3:` 目标名不同则自动追加文件名

## 配置文件

**各配置键的完整说明以示例模板注释为准**（`./kvs demo` 生成后直接阅读 `kvs.ini`；`[headers]` 请求头改写、`[service.<name>]` 服务自动部署均见模板注释；`mirror` 为独立子命令，不读配置文件）。

值模板语法（所有值支持）：

| 语法 | 说明 |
|---|---|
| `{VAR}` / `{VAR:-default}` | 取环境变量 VAR，空则返回空 / default |
| `${VAR}` / `$VAR` | 标准 os.ExpandEnv |
| `@now` | 运行时时间戳（仅 text 后端），RFC3339 |
| `{KVS_SVC_<NAME>_*}` | `[service.<name>]` 反推变量（HOME/ADDR/HOST/PORT/SOCK/VERSION/VERHASH/BIN_HOME），见模板注释与 [agents.md](agents.md) |

### 路由 `[proxies.<name>]` 段（必填）

每行一个路由 `prefix = url`，**按文件顺序匹配**。一个配置文件可定义多张命名路由表：`[proxies]`（无名表）与 `[proxies.<name>]`，`-n :<name>` 选择其一，默认用第一张表。

前缀标记：普通 `/path/` 前缀匹配；`^pattern` 正则；`http(s)://`、`ws(s)://` 开头为全域名匹配；`cc~/`、`ws~/` 性能标记见下文；前缀收尾的 `[k=v]` 是**单条件 query 门禁**——路径命中后还要求请求带上这一对参数（按解码后的值精确相等，缺参/不等即视为未命中），继续尝试后续路由（同前缀的无门禁路由即兜底）。只支持一个条件（效率取舍）；门禁可与 `cc~/ws~/^` 组合，对该行逗号展开的全部前缀生效；括号内容含逗号或没有 `=`（如正则字符类 `[0-9]`）就整体按字面路径处理。例：`/remote/vscode[akey=vscode-remote]=https://vscode.dev/` 只放行 `?akey=vscode-remote` 的请求（query 原样转发到后端）。

| scheme | 说明 |
|---|---|
| `http`/`https`、`ws`/`wss` | 反向代理到 HTTP 后端（升级请求透传；`ws`/`wss` 等同于 `http`/`https`） |
| `unix` | Unix domain socket |
| `file` | 静态文件服务器（br/gz/zst 预压缩优选） |
| `text` | 直接返回文本；支持 `@now` 替换当前时间（RFC3339） |
| `svc://<name>` | kvs 托管服务：按 `[service.<name>]` 段探活→下载→解压→启动，再反代到 `address`（准备期间返回 loading 页） |
| `relay://` / `api://` | 进程内中继（target 只认 `vscode`（VS Code tunnel 中继）与 `zcode`（配对中继））/ handler 注册表（见 [agents.md](agents.md)） |

### 托管服务 `[service.<name>]` 段

`svc://<name>` 路由触发对应服务段的解析与生命周期管理（**未被选中表 `svc://` 引用的 `[service.<name>]` 段保持休眠，完全不解析**）：`healthz`（`#@<code>` 后缀要求精确状态码，否则只要求网络可达）探活 → 版本解析（`version` / `version_latest_url` / `version_hash_url`，支持 `/__restart?v=` 覆盖）→ 下载（`download`）→ 解压（`bin_home`）→ `once_shell`/`init_shell` → `command` 启动；kvs 退出或 `/__restart` 时执行 `stop_shell`。外部已运行的服务（探活通过）只代理、不托管。

每服务一组环境变量（`<NAME>` 为服务名大写，**由 kvs 反向赋值**，可在路由与后续配置中引用）：

| 变量 | 说明 |
|---|---|
| `KVS_SVC_<NAME>_HOME` | 工作目录，未指定默认 `{KVS_HOME}/<name>` |
| `KVS_SVC_<NAME>_ADDR` | 后端地址（`http(s)://` 或 `unix://`），由 `address` 解析回填 |
| `KVS_SVC_<NAME>_HOST/_PORT/_SOCK` | 由 `_ADDR` 反推（http 取 host/port，unix 取 socket 路径） |
| `KVS_SVC_<NAME>_VERSION/_VERHASH` | 解析出的版本号/哈希 |
| `KVS_SVC_<NAME>_BIN_HOME` | 解析出的程序目录 |
| `envs_default` | 环境变量默认值（JSON：变量名→默认值），系统变量为空时填充、不覆盖已有值；段内最后处理，可引用上述服务变量 |

VS Code 专有键（`vsc_language`、`vsc_agents_*`、`vsc_open_url`、`actions`）放在对应服务段内，作用于该服务的 agent/注入流程。

## WebSocket 升级（性能与正确性）

升级请求走**自研裸连接隧道**（`pkg/wsproxy.go`），不再使用标准库的升级分支：

- 两端都是裸 socket 时用**内核 splice** 搬运数据（标准库因上游 body 是包装类型，结构上用不到 splice），大帧（文件传输、端口转发）用户态 CPU 明显下降；
- 补齐握手期间已被 `http.Server` 缓冲的客户端字节——客户端把首帧与握手写在同一个 TCP 段时，标准库会**丢掉这一帧**，kvs 现在不会；
- 可选空闲超时、TCP keepalive；握手任何阶段失败自动回退标准库路径（非升级请求始终走标准库反代，行为不变）。

| 配置键 | 默认 | 说明 |
|---|---|---|
| `ws_proxy` | `auto` | `auto`=快路径（失败回退）；`stdlib`=始终旧行为 |
| `ws_idle_timeout` | `0`（关闭） | 秒；两个方向都静默约 2 倍该值才断开 |

`ws_idle_timeout > 0` 时的两种断开（只影响这一条隧道）：

- **空闲**：两个方向都没有数据流动约 2×`ws_idle_timeout` 秒 → 断开（回收黑洞连接）；
- **对端停滞**：某一方向有数据可读，但对端在 `max(ws_idle_timeout, 30s)` 内一个字节都没读走
  （典型：客户端挂起、不再读取）→ 立即断开。这里**不会**"续期后继续拷贝"：写超时时内核
  splice 管道（或用户态缓冲）里已从源读出、还没写出去的字节会随之丢失，继续拷只会让对端
  拿到一段悄悄缺了字节的流；断开则对端看到的是明确的连接结束（客户端会自行重连）。

## 反向代理调优

| 配置键 | 默认 | 说明 |
|---|---|---|
| `proxy_idle_conns_per_host` | `32` | 每后端空闲连接上限（Go 默认只有 2） |
| `proxy_max_idle_conns` | `256` | 全局空闲连接上限 |
| `proxy_disable_compression` | `true` | 原样透传 `Accept-Encoding`，代理不做透明 gzip |
| `proxy_flush_interval` | `100` | 流式响应刷出间隔（毫秒）；`0`=不主动刷，`-1`=每次写都刷 |
| `read_timeout` | `60` | 主 server 读完整请求（含 body）的秒数；`0`=不限（大上传/慢客户端），负值告警保持默认。WebSocket 不受影响（hijack 清空 deadline） |

响应体拷贝缓冲是**池化**的（`httputil.BufferPool`，32KB）：标准库默认每个带 body 的响应
都新分配一块 32KB，池化后稳态零分配（实测 1MiB 响应 47.5KB/op → 15.7KB/op，见
`pkg/transport_test.go` 的 benchmark）。

## 运行观测

- `/__stats`：JSON 计数（WS 隧道 splice/generic 数量与字节、中继帧数、缓存命中、goroutine/堆），受全局鉴权保护。
- `KVS_PPROF=1`（或 `=<port>`、`=<addr>`）：额外起一个 pprof 监听，默认只绑 `127.0.0.1`，默认关闭。

## 外部资源代理 (`/__cache/`)

需同时设置 `cache_dir`（磁盘目录）和 `cache_path`（路由前缀，默认空=禁用，设为 `/__cache/` 启用）。

```
/__cache/[cc~]{scheme}:{host}[/path][?query]
```

`cc~` 前缀 = 缓存（仅 GET 2xx 与 404），布局 `{cache_dir}/cache/ccproxy/{scheme}:{host}/path`。
缓存键（`{scheme}:{host}` 与全路径）**统一小写**，`/GitHub/x` 与 `/github/x` 是同一条目；
只折叠**磁盘键**，回源请求（Host 与 path）**保持原大小写**，大小写敏感的上游不受影响。
无扩展名的请求归一到 `__index`（目录首页与 `…/0.42.0/extension` 这类无后缀端点）。`cache_sed`
缓存内容替换（每个 `KVS_CC_SED_<名称>` 变量一条规则，格式 `old|new|file[,files…][|host[,hosts…]]`，
含 once/each 动态 Host 模式）见 demo 模板注释与 [agents.md](agents.md)。

`[cache_headers]` 段给缓存响应附加固定头（只在 `cache_path` 前缀下生效，普通路由不受影响），
用于跨域加载静态资源：如 `Access-Control-Allow-Origin = *`、`Cross-Origin-Resource-Policy = cross-origin`。
`cache_path` 前缀下的 `OPTIONS` 预检直接回 204（带同一组头），不会触达缓存或上游；
上游响应里同名的头会被丢弃，以配置为准（不会重复）。

缓存**命中**按持久化的 sed 计划选择发码方式：`each`（需按请求重写）读内存重写；其余模式
（无计划/none/once）**文件直出**——`http.ServeContent` 走内核 sendfile（零拷贝、零堆分配），
附带 `Content-Length`、`Accept-Ranges`、Range/If-Modified-Since 支持。元数据（`_.json`）有
内存缓存，按 size+mtime+inode 判失效（同秒重写也不会漏）。

## HTTPS / TLS

`use_ssl = true`：ECDSA P-256 自签名证书，HTTPS 端口 = `port + 1`。

## 已知限制

- `command` 不支持带空格的参数
- 外部代理缓存无大小限制

## 感谢

- [Go](https://go.dev/) 标准库 —— 零第三方依赖，全部能力的基础
- [Visual Studio Code](https://code.visualstudio.com/) 及其开源生态 —— mirror 同步、Update 菜单注入、web-extension 资源本地化的服务对象；简体中文语言包来自 [MS-CEINTL/vscode-language-pack-zh-hans](https://github.com/MS-CEINTL/vscode-language-pack-zh-hans)
- [ZCode](https://z.ai/)（Z.ai 桌面端）—— zcodex 应用中心接入与中继的远程控制目标
- S3 兼容存储生态（MinIO 等）—— `mirror` / `syncto` 的存储后端
