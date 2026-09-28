# kvs — 反向代理网关

kai vscode

轻量级 Go 反向代理。提供 **Cookie 认证**、**多后端路由（前缀/正则）**、**服务自动部署**、**外部资源代理（带缓存）**、**退出按钮 + Update 菜单注入（VS Code专有）**、**S3 镜像同步（VS Code专有）**。仅依赖 Go 标准库 + `embed`，无第三方依赖，**完全通过 `kvs.ini` 配置文件驱动**。

## 快速开始

```bash
make build
./kvs help                          # 查看帮助
./kvs demo                          # 生成示例配置 ./kvs.ini（kvs.default.ini 模板）
./kvs demo vscode                   # 生成示例配置 ./kvs.ini（kvs.vscode.ini 模板）
# 编辑 kvs.ini —— 各键说明见模板内注释
./kvs -c kvs.ini                    # 指定配置文件启动
./kvs -c default|vscode|zcodex      # 内置预设，无需磁盘文件（zcodex=应用中心，见 agents.md）
./kvs -n "/=http://127.0.0.1:8080"  # 内联路由，自动补充 -c default
```

**`-c` 为必填项**，不指定直接报错退出。`-n` 用 `;` 分割多条 `prefix=url` 替代 `[proxies]` 段（支持 `&` 前缀），自动补充 `-c default` 并禁用 `[service]`；环境变量 `KVS_PROXIES` 与 `-n` 等价。`-c zcodex` 应用中心（`api://manager`）的页面功能与内部机制见 [agents.md](agents.md)。

## 子命令

| 命令 | 说明 |
|---|---|
| `kvs help` | 显示帮助信息（也支持 `-h`、`--help`） |
| `kvs demo [default\|vscode]` | 生成示例配置文件 `kvs.ini`（默认 default 模板） |
| `kvs mirror -c <config> [version]` | 同步 VS Code 版本到 S3 兼容存储（配置见 `demo vscode` 模板 `[mirror]` 段） |
| `kvs syncto <src> <dst>` | 本地 ↔ S3 文件同步（见下方说明） |

`kvs syncto` **只读环境变量，不读 kvs.ini**，缺少必需变量直接报错：`KVS_S3_PREFIX`（S3 兼容服务基础 URL，bucket 根）、`KVS_S3_ACCESS`、`KVS_S3_SECRET` 必需，`KVS_S3_REGION` 可选（默认 us-east-1）。

```bash
kvs syncto /zcode s3:/vsc/zcode   # 上传：本地文件/目录 → S3（递归，同名覆盖）
kvs syncto s3:/vsc/zcode /zcode   # 下载：S3 → 本地（对象或前缀递归）
```

- `s3:` 与本地路径二选一（`s3:/bucket内的key路径`）；不支持两个 S3 对拷
- 目录按相对路径 1:1 映射为对象 key；单文件时 `s3:` 目标名不同则自动追加文件名

## 配置文件

**各配置键的完整说明以示例模板注释为准**（`./kvs demo` 生成后直接阅读 `kvs.ini`；`[headers]` 请求头改写、`[service]` 服务自动部署、`[mirror]` S3 镜像（VS Code 专有）均见模板注释）。

值模板语法（所有值支持）：

| 语法 | 说明 |
|---|---|
| `{VAR}` / `{VAR:-default}` | 取环境变量 VAR，空则返回空 / default |
| `${VAR}` / `$VAR` | 标准 os.ExpandEnv |
| `@now` | 运行时时间戳（仅 text 后端），RFC3339 |
| `{SVC_HOME}` 等 | [service] 内部变量，见模板注释与 [agents.md](agents.md) |

### 路由 `[proxies]` 段（必填）

每行一个路由 `prefix = url`，**按文件顺序匹配**。前缀标记：普通 `/path/` 前缀匹配；`&/path/` kvs 托管服务（触发自动部署、loading 页，**必须显式标记**）；`^pattern` 正则；`http(s)://`、`ws(s)://` 开头为全域名匹配。

| scheme | 说明 |
|---|---|
| `http`/`https`、`ws`/`wss` | 反向代理到 HTTP 后端（升级请求透传；`ws`/`wss` 等同于 `http`/`https`） |
| `unix` | Unix domain socket |
| `file` | 静态文件服务器（br/gz/zst 预压缩优选） |
| `text` | 直接返回文本；支持 `@now` 替换当前时间（RFC3339） |
| `wsws://` / `api://` | 进程内 zcode 中继 / handler 注册表（zcodex 预设使用，见 [agents.md](agents.md)） |

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

需同时设置 `cache_dir`（磁盘目录）和 `proxy_path`（路由前缀，默认空=禁用，设为 `/__cache/` 启用）。

```
/__cache/[cc~]{scheme}:{host}[/path][?query]
```

`cc~` 前缀 = 缓存（仅 GET 2xx 与 404），布局 `{cache_dir}/cache/ccproxy/{scheme}:{host}/path`。`cache_sed`
缓存内容替换（规则格式、once/each 动态 Host 模式）见 demo 模板注释与 [agents.md](agents.md)。

缓存**命中**按持久化的 sed 计划选择发码方式：`each`（需按请求重写）读内存重写；其余模式
（none/once/旧条目）**文件直出**——`http.ServeContent` 走内核 sendfile（零拷贝、零堆分配），
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
