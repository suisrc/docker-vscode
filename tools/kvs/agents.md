# agents.md — kvs 开发文档（AI 向）

面向 AI 协作开发的内部机制与约定速查。用户文档（简介/快速开始/使用/感谢）在 [readme.md](readme.md)；**各配置键的完整说明以 demo 模板注释为准**（`kvs demo` 生成，即 `pkg/kvs.ini`）。分工：readme=使用说明，demo 模板=配置参考，本文件=架构与开发。两文档与代码冲突时以代码为准。

## 1. 定位与硬约束

- 单二进制 Go 反向代理：**仅标准库 + `embed`，禁止引入第三方依赖**
- 行为全部由 kvs.ini（ini + 值模板展开）驱动；启动 `-c` 必填
- 交付形态：`make build` → `./kvs`，静态资源全部内嵌
- 生产实例归 owner 所有：**不得 kill/重启非自己启动的进程**；部署与升级只由 owner 决定

## 2. 代码地图

| 文件 | 职责 |
|---|---|
| `main.go` | CLI 分发（`help`/`demo`/`mirror`/`syncto` 子命令；无子命令=代理模式）、mux 装配、登录/内置端点、注入脚本服务 |
| `pkg/config.go` | ini 解析 + 值模板展开（含多行 JSON 修复）、多路由表 `[proxies.<name>]`（`-n :name` 选表）、多服务段 `[service.<name>]` + `KVS_SVC_<NAME>_*` 环境变量反推、版本覆盖（`SetOverrideVersion` 按服务）、`-c` 可选（缺省=内嵌 kvs.ini，仅接受文件路径） |
| `pkg/backend.go` | `Backend` 抽象；`CreateBackendHandler` 按 scheme 分发；`registerAPI`（api:// 进程内注册表）；ws 代理；`precompressedFileServer`（br/gz/zst 预压缩静态） |
| `pkg/service.go` | service 生命周期：检测→版本解析→下载→解压→脚本→启停；`ServiceManager`（每 svc:// 服务独立 ServiceState+Process）；healthz `#@code` 探活 |
| `pkg/cache.go` | cc~ 缓存核心：`/__cache/` 外部资源代理（命中按 sed 计划直出 sendfile/内存重写，元数据内存缓存 size+mtime+inode 判失效）、两条入口、透传；规则匹配小工具（hostMatch/globMatch/targetHost/splitList） |
| `pkg/cachesed.go` | cache_sed（`KVS_CC_SED_<名称>`）：正文改写 once/each/none 计划与 `_.json` 元数据、brotli/gzip 解压编码 |
| `pkg/cacherec.go` | cache_rec（`KVS_CC_REC_<名称>`）：命中即短路的响应规则（`file://` 正文可由文件提供） |
| `pkg/server.go` | 服务器构建（HTTP + HTTPS=port+1 自签 ECDSA P-256）、静态资产/登录页服务、`isBrowserRequest` |
| `pkg/vscode.go` | VS Code 专有：web-extension-resource 本地化（上游乱码 issue microsoft/vscode#299425）、语言包/汉化链路 |
| `pkg/relay.go` | `relay://` 后端公共层：target→handler 注册表（各实现 init() 自注册，backend.go 查表分流；未注册 target 构建期报错）+ 渠道 ID 承接（`SetTunnelID`，tunnel_id/KVS_TUNNEL_ID，目前仅 vscode 实现消费）+ 共享小工具（`hijackRawConn`：hijack 后清绝对 deadline+开 15s TCP keepalive；`clientRemoteAddr`：XFF 首值优先、`::ffff:` 还原） |
| `pkg/vscrelay.go` | `relay://vscode`：Remote-Server tunnel 中继。worker（kind=worker 升级）与 client（?tkn= 路由）共用单端点；文本帧=控制 JSON（ready/open/version 探测/pause/resume/close），二进制帧=[1B idLen][id][payload] 会话数据；client 腿全原始字节；背压端到端（client 侧 staging 高水位+pause/resume、通道写积压高水位+TCP 背压）；探测判死、设备独占、掉线保留期；无空闲超时（空闲是隧道常态）；tunnel_id 配置非空时校验 worker tkn 第三段并经 ready 回告 |
| `pkg/vagent.go` | agent 端点：`vsc_agent_cmds` 预设表（JSON）、端点目录扫描（*.json） |
| `pkg/zcorelay.go` | `relay://zcode` 进程内配对中继（设备注册/心跳/远程控制，标识符统一 zco 前缀）；`zcoStore`（zcodex.json 持久化 + 旧版迁移 + 防抖原子落盘）；`KVS_ZCODEX_NODE` 懒启动 |
| `pkg/manager.go` | 应用中心：合并视图组装、REST API、在线探测（45s 缓存）、`registerAPI("manager")` |
| `pkg/manager.html` | 管理页单文件前端（客户端渲染，PC 卡片/移动行双布局） |
| `pkg/wsconn.go` | 最小 RFC 6455 WebSocket 服务端（stdlib 实现）；帧写单次 writev、写超时粗粒度刷新、读缓冲复用、unmask 字长化；升级即接 `hijackRawConn`（长命连接卫生） |
| `pkg/wsproxy.go` | WebSocket 升级快路径：自研裸连接隧道（splice、补齐握手期缓冲字节、可选空闲超时、失败回退 stdlib）；客户端侧 hijack 后同样清 deadline+保活 |
| `pkg/transport.go` | 反代 Transport 调优（空闲连接/透传 Accept-Encoding/FlushInterval）+ 响应体拷贝缓冲池（`BufferPool`，省掉每个响应的 32KB 分配），按后端类型缓存 |
| `pkg/jsonhead.go` | 只读 JSON 顶层头部的零分配扫描器（中继 data 帧快路径判定） |
| `pkg/stats.go` | 运行计数（`/__stats`）与可选 pprof（`KVS_PPROF`） |
| `pkg/*_test.go` | 单元测试与微基准：帧读写/unmask/JSON 头部扫描；隧道（写停滞致命、慢读者存活、空闲判定、流水线首帧端到端、后端不可达回退）；反代响应路径 benchmark（`transport_test.go`） |
| `pkg/proc.go` | 子进程 Setsid 脱离、终止时前台进程组恢复 |
| `pkg/s3mirror.go` | `mirror`/`syncto` 的 S3 同步实现（owner 开发中，勿擅自重构） |
| `pkg/help.go` | help 文本——新增配置项/环境变量必须同步 |
| `pkg/` 资产 | `manager.html` `login.html` `loading.html` `logout.vsc.js` `kvs.ini` `favicon.ico`，`go:embed` 进二进制 |

## 3. 核心机制

### 3.1 值模板展开（config.go）

`{VAR}`、`{VAR:-default}`、`${VAR}`/`$VAR`（os.ExpandEnv）、`@now`（仅 text 后端，RFC3339）。`[service.<name>]` 值可引用反推的 `KVS_SVC_<NAME>_*` 变量，段内解析序：home→address→healthz→version→download→bin_home→scripts/command；`download` 支持 VERSION/VERHASH 裸词替换，`version_hash_url` 支持 VERSION 替换（裸词与 `{}` 形式均可，替换发生在版本解析之后）。

### 3.2 路由与后端协议

路由表：`[proxies.<name>]` 命名多表（`[proxies]` 无名表），`-n :name` 选择，默认取文件中第一张表。每行一个路由，按文件顺序匹配。前缀标记：普通前缀；`^`（正则）；`http(s)://`、`ws(s)://` 开头（全域名）。

后端 scheme 全集（readme 仅列用户常用项）：

| scheme | 实现 |
|---|---|
| `http`/`https` | 反向代理（httputil.ReverseProxy） |
| `ws`/`wss` | 同 http/https（`rewriteProxyRequest` 会把 ws→http、wss→https，否则 Transport 报 `unsupported protocol scheme "ws"`） |
| `unix` | Unix domain socket |
| `file` | 静态文件（含 br/gz/zst 预压缩优选） |
| `text` | 直接返回文本，支持 `@now` |
| `svc://<name>` | kvs 托管服务：`[service.<name>]` 段，按地址反代；scheme 即显式服务标记 |
| `relay://<target>` | 进程内中继，target 只认注册表预置的两个：`vscode`（Remote-Server tunnel 中继，vscrelay.go）与 `zcode`（配对中继，zcorelay.go）；其他 target 构建期报错 |
| `api://<name>` | 进程内 handler 注册表（`registerAPI`），目前仅 `api://manager` |

多托管服务（svc://）：LoadInitConfig 先扫选中表的 `svc://<name>` 引用（主服务=第一个引用），**只解析被引用的服务段**（未引用的 `[service.<name>]` 段保持休眠：不反推环境变量、不解析版本、不注册），被引用段按依赖序解析并**反向赋值** `KVS_SVC_<NAME>_*` 环境变量（HOME 默认 `{KVS_HOME}/<name>`；ADDR 回填；HOST/PORT 由 http 地址、SOCK 由 unix 地址反推；VERSION/VERHASH 解析后回填；BIN_HOME 回填；段末处理 envs_default——系统环境变量为空时用默认值填充，默认值可引用全部已反推变量），然后才展开路由值——路由里 `{KVS_SVC_ZCODE_BIN_HOME}` 这类引用由此生效。每个 svc:// 服务有独立的 ServiceState（loading 页）与 Process（Setsid 子进程组），启动命令在配置加载期已完成 `{KVS_SVC_<NAME>_*}` 展开，子进程继承完整进程环境；`/__restart?svc=<name>`（缺省=主服务）停进程+stop_shell+清 unix socket 并重置该服务状态；kvs 退出时 `StopAll()` 只杀 kvs 托管的进程。版本覆盖 `SetOverrideVersion(svc, v)` 按服务记录。

service 生命周期（service.go `PrepareService(sc)`）：探活（`healthz` 键=`#@<code>` 后缀要求精确状态码、无后缀只要求可达；unix 探活失败会清理残留 socket 文件）→ 已存活=外部服务（只代理不托管启停）→ `bin_home` 非空跳过下载 → 解析下载地址（`download` 优先，否则 `download_info`+`download_field_url`）→ 跟随重定向取扩展名（缓存文件名后缀）→ 下载到 `{cache_dir}/version/{service}/{version}_{version_hash}.{ext}`（按服务名分目录，避免不同服务同版本冲突） → 解压到 `bin_home` → 执行 `once_shell`（`{bin_home}/__once__` 标记）+`init_shell`；`stop_shell` 仅 kvs 托管的后端退出时执行。

### 3.2.1 WebSocket 升级快路径（pkg/wsproxy.go）

标准库 `httputil.ReverseProxy` 的升级分支有三个问题，因此升级请求改由自研隧道处理
（`ws_proxy=auto`，默认；`stdlib` 一键回退）：

1. 双向都是 32KB 用户态缓冲拷贝：上游 body 是 `*httputil.readWriteCloserBody` 包装类型，
   而 `net` 的 splice 只认裸 `*net.TCPConn`/`*net.UnixConn`，结构上用不到内核 splice；
2. hijack 后不消费 `http.Server` 已缓冲的客户端字节（`brw.Reader`）——客户端把首帧与握手
   写在同一个 TCP 段时**首帧被静默丢弃**（已实测复现）；
3. 隧道没有空闲超时。

实现要点：自己 `net.Dial`（tcp/unix，https/wss 再包 TLS）→ `req.Write` → `http.ReadResponse`；
非 101 响应按普通响应回写（含 `login_authz` 修饰链）；101 则 hijack 客户端、**两侧都先 drain
缓冲字节**（drain 半截失败即放弃隧道，否则帧序错位）、再在裸 conn 之间 `io.Copy`
（socket↔socket 命中 `internal/poll.Splice`，内部借池化 pipe）。非升级请求始终走标准
`httputil.ReverseProxy`，请求构造与 Director 共用 `rewriteProxyRequest`（含 `X-Forwarded-For` 补齐）。

空闲超时（可选，`ws_idle_timeout>0`）：分块 `io.CopyN` + 每方向读/写 deadline，两种断开：

| 情形 | 判据 | 行为 |
|---|---|---|
| 空闲 | 两个方向都静默 2×idle（本方向无数据且对方向计数未变，连续两个窗口） | 断开 |
| 对端停滞 | 有数据可读但对端 `max(idle,30s)` 内没读走一个字节（写 deadline = 读 deadline + 宽限，靠"写 deadline 是否已过"区分） | 立即断开 |

**写超时必须致命**：`io.CopyN` 返回前，已从源读出但没写出去的字节会随 splice 管道/用户态缓冲
丢弃，而返回的 n 只统计写出去的字节——判活续期 = 静默截断（`temp/audit/loss` 实测：8MiB 流量
静默丢 1.8MiB，对端流里出现空洞）。读超时（源侧无数据）没有半成品数据，续期才是安全的。

ws~ 路由前缀匹配与退避：`ExactPathMatch` = `ws~` 且目标非隧道类 scheme（http/https/ws/wss/unix）
才按前缀匹配（这样一个 ws~ 路由能覆盖整棵子树）；进程内端点（`relay://`、`api://`）仍要求路径完全
相等。已核对 vscode 预设（只有 `&/` unix 与 `/__healthz`）与 zcodex 预设（`ws~/zcode/remote/ws=relay://`
仍是精确匹配），线上路由不受该变更影响。

基准（`temp/bench/`，300×1MiB 回环，经 kvs）：**用户态 CPU 21 ticks vs stdlib 44 ticks（-52%）**，
吞吐 177 vs 166 MiB/s，64B 小帧 RTT 无差异（小帧路径本就只有 1 read + 1 write，没有空间）。

### 3.3 Cookie 认证（main.go / server.go）

两阶段。**请求阶段拦截**（`login_token` 非空时生效）：所有非 `/__` 前缀路径校验 Cookie，内置端点与 `/__cache/` 始终放行；常数时间比较防时序攻击。**响应阶段重定向**（需 `login_authz=true`）：后端返回 401/403 → 替换为登录页；`/__logout` 清除 Cookie。注意：`login_token` 为空时请求阶段无拦截，`login_authz` 仅在后端自身返回 401/403 时生效。`login_timeout`：

| 值 | Cookie 格式 | 校验 | 过期 |
|---|---|---|---|
| `0`/空 | `login_token` 明文 | 直接比对 | session |
| `>0` | `<hash>.<ts>.<salt>`，hash=`sha256(ts+salt+login_token)[:24]`，salt=16 位随机 hex | 哈希比对 + 过期检查（过期令牌拒绝，未来时间戳拒绝） | 剩余 ≤1/4 时自动续期（滑动过期） |
| `<0` | 启动报错 | | |

### 3.4 `/__cache/`（cache.go）

- URL：`/__cache/[cc~]{scheme}:{host}[/path][?query]`；`cc~` 前缀=缓存（仅 GET 2xx）
- `[cache_headers]`：给缓存响应附加固定头（CORS 等），**只在 `cache_path` 前缀下生效**——
  普通路由与路由级 cc~ 不受影响（上游自己的同名头也照原样透出）。设置点在任何写入之前，
  所以 HIT/MISS/透传/短路/错误响应都带；上游响应里同名的头会被丢弃（不会变成两个值）。
  `cache_path` 下的 `OPTIONS` 预检直接回 204（带同一组头），不读缓存也不发上游。
  遗留：非 GET 请求不剥 `cc~` 标记，透传只认 `/__cache/{scheme}:{host}/…`（GET 才认 `cc~`）。
- cache_sed 规则：每个 `KVS_CC_SED_<名称>` 环境变量一条（名称为分组标签，按名排序生效；无 ini 配置项），格式 `old|new|file[,files…][|host[,hosts…]]`：files 内任一命中即套用，`__index`=无扩展名的请求（目录首页与 `…/0.42.0/extension` 这类无后缀端点统一存为 `__index`），每个模式一个 `*` 通配；host 列表可选（空=任意，比对**代理后端的主机**即内容来源如 vscode.dev/main.vscode-cdn.net，与访问域名无关；端口/大小写无关、支持一个 `*`），host 过滤与模式一样在**写入时**判定；new 中 `>host<` 展开为当前请求 Host（浏览器可见地址，与 host 过滤不是一回事）
- 模式在**首次写入缓存时**判定并持久化到元数据，之后命中只按元数据处理（不反查运行时规则）：
  - `once`：new 与请求无关 → 替换烤入存储体，原文备份 `<file>_.bak1`（仅一次），命中直接返回
  - `each`：某条替换含 `>host<` 且原文存在 old → once 规则先烤入，原始内容明文存储，each 数组存 `_.json` 的 `sed.each`；命中按当前 Host 重替换，gzip 跟随原始响应（`src_gzip`）
  - `none`：匹配文件名但内容未变 → 原样存取
  - 同一文件可同时有 once+each：once 先替换保存，文件标记 each，每请求补 each 部分
- 布局：`{cache_dir}/cache/ccproxy/{scheme}:{host}/path`（存储体）、`path_.json`（status/headers + sed）、`path_.bak1`；**缓存键整体小写**（`{scheme}:{host}` 与全路径），域名/路径大小写差异不会拆成两条目或造成未命中（仅磁盘键折叠，上游请求仍按原大小写发出）
- 无扩展名的请求统一归一到 `__index`（目录首页 `/`、`/0.42.0/`（尾斜杠）与 `…/0.42.0/extension` 这类无后缀端点），规则只需匹配 `__index`
- cache_rec 规则：每个 `KVS_CC_REC_<名称>` 环境变量一条（全局生效，两条缓存入口都查，先于缓存查找），格式 `upHost|path[,paths…]|status[|content-text[|content-type]]`；host 与 cache_sed 同一套匹配（空=任意），path 是**子串**匹配且多个**全部满足**（AND，与 files 的任一命中相反），正文类型默认 text/plain，正文以 `file://` 开头则每次命中读取该文件内容（读不到回 500）；命中即短路：不发上游、不读写缓存，响应带 `X-Cache: REC`
- 命中发码：`each` 读内存按 Host 重写（唯一还读整份正文的情况）；无计划/none/once **文件直出**
  （`http.ServeContent` → 内核 sendfile，附 Content-Length/Accept-Ranges/Range/If-Modified-Since，
  Last-Modified 用缓存文件 mtime——条件请求针对"缓存条目是否变化"作答）；非 200 状态（404 也
  入缓存）退回内存路径保状态码。元数据读入有内存缓存（`ccMetaCache`，上限 4096 条，整表丢弃
  式淘汰），以 size+mtime+**inode** 判失效——原子写是 temp+rename 必换 inode，同秒重写不漏判。
- MISS 会先 `WriteHeader(resp.StatusCode)` 再流式转发（上游 404 入缓存时状态码不能被隐式
  200 吞掉）。`/__stats` 的 cache.hits/misses 在这两处推进。

### 3.4.1 中继帧路径（wsconn.go / zcorelay.go）

- 每帧 syscall 5→2：header 用栈上数组 + `net.Buffers.WriteTo`（TCP/Unix 一次 writev），
  小帧合并进 `wsConn.wbuf` 一次写出；`writeMu` 由 channel 换成 `sync.Mutex`。
- 写超时不再每帧两次 `SetWriteDeadline`，改为"距上次设置超过一半才刷新"（等价语义，
  稳态零额外 runtime 调用）。
- 读路径复用连接级 `rbuf`（≤1MiB；更大的帧单独分配），稳态零分配；hijack 后若读缓冲为空
  则换成 64KB bufio；unmask 按 64 位字长 XOR。
- **data 帧快路径**：`jsonHeadStringIs(frame,"type","data")` 只扫到顶层 `type` 成员就返回，
  跳过整帧解析（1MiB 帧：**51ns vs 4.9ms**，且零分配）；判不出来一律回退完整 `json.Unmarshal`，
  语义只在"声明为 data 但整体非法 JSON 的帧会被原样转发"这一点上更宽松（符合中继逐字节转发的契约）。

### 3.5 zcodex 模式与 manager（zcorelay.go / manager.go）

`-c` 可选且只接受文件路径（缺省=内嵌 `kvs.ini`）；路由表只有 `-n :name` 一种选法，无预设别名。代码不再注入任何 zcodex 环境默认值：`KVS_PATH_PUBLIC`（桌面客户端无法走 Cookie 认证）、`KVS_CC_SED_*`（cache_sed 规则，wss 地址与 CDN 地址重写）由部署环境/Makefile zcodex 目标或 `[service.zcode] envs_default` 提供（值见 `[service.zcode]` 注释）。

管理页 `manager.html` 挂载于 `/=api://manager`（客户端渲染，支持深浅色主题）。条目四类：`[ZCD]local` 本机 zcode（`/zcode`，kvs 懒启动，始终可点）；`[ZCD]xxx` 通过 relay 中继注册的远程 zcode 桌面端（在线时点击打开远程控制终端）；`[VSC]zzz` VS Code 应用：`relay://vscode` 的隧道设备实时合成（id `vsc-tun-<device>`，在线时点击打开 `<挂载前缀>?tkn=<token>`，掉线只保留徽章；删除卡片会清隧道占用，worker 重连即回来；tags/visit 以 stub 记录存 zcodex.json，隧道过期不留幽灵卡）+ 手动添加的远程卡（同一设备的不同工作区以快捷方式挂同一卡片，访问地址 `{卡片同host}/?folder=<工作区路径>`，可编辑/删除）；`[APP]`/`[LNX]` 「其他」类型设备（Logo 四选一 vsc/zcd/linux/other）。卡片顺序固定（不按访问时间排序），拖拽调整后持久化到 `order` 段，新条目追加末尾。

合并视图条目来源（`mgrBuildEntries`）：
1. `zcd-local`：本机 zcode，URL=`/zcode`（相对，始终可点）
2. 在线中继设备：URL=`/remote/v4?sid&hash&t&mid&name&app_version` —— **相对 path 契约：后端只给 path，origin 由前端 `window.open` 按当前 `location.origin` 补全；禁止服务端拼 scheme/host（`mgrProto` 已删除）**。`pass_hash` 只存在于链接内，不作为 API 数据字段下发
3. 自定义应用（zcodex.json `apps` 段）

探测：http(s) 目标请求 `<base>/__version` 取版本+在线，缓存 45s（`mgrProbeTTL`）。

持久化 `{KVS_HOME}/zcodex.json`：`version/devices/apps/order/tools/notes` 段；旧格式自动迁移；1s 防抖原子落盘（notify 通道触发）。

manager API（随页面一同鉴权）：

| 接口 | 说明 |
|---|---|
| `GET /__manager` | 合并视图：默认应用 + 中继设备 + 自定义应用（含版本/系统/在线探测，45s 缓存；内置条目 `url` 均为相对 path，由页面按当前 origin 打开） |
| `POST /__manager` | 添加自定义应用 `{type,icon?,name,url,folders?}`，type=vsc / other（Logo：vsc/zcd/linux/other） |
| `POST /__manager`（带 `id`） | 编辑自定义应用（名称/地址/Logo/工作区），仅自定义应用可编辑 |
| `DELETE /__manager?id=` | 删除自定义应用（默认应用与中继设备不可删；同步清除其 notes） |
| `POST /__manager/visit` | 点击打开时上报 `{id}`，记录首次接入/最近访问 |
| `POST /__manager/order` | 保存拖拽顺序 `{ids}` → `order` 段；新条目追加末尾 |
| `POST /__manager/tags` | 条目自定义标记 `{id,tags}`（≤6 个；在线状态为自动标签不可删） |
| `POST /__manager/tool` / `DELETE /__manager/tool?name=` | 底部工具栏增改删 `{name,addr,old?}` |
| `POST /__manager/folder` / `DELETE /__manager/folder?id=&name=` | 工作区快捷方式增删（仅 VS Code 应用，`{id,name,path}`，打开为 `{base}/?folder=<path>`） |
| `GET /__manager/note?id=` / `POST /__manager/note` | 备注读取/保存清除 `{id,note}`（独立接口不随列表返回；≤500 字；存 `notes` 段，空串=删除） |

manager.html 约定：
- 767px 断点：PC 卡片网格 + 底部工具栏；移动列表行（点击展开工作区）。**移动行 Logo 纯展示（`logoNode`）、无编辑/删除；备注按钮（`noteLogo`）与编辑/删除仅 PC 卡片**
- 弹窗拉伸：蒙层带 `.xw` 标记（拖右下角把手），备注弹窗双向 resize + flex 填充；移动端一律禁拉伸（底部抽屉整宽）
- 拖拽排序：pointer 事件，触屏长按 240ms / 鼠标位移 6px 阈值；`suppressClick` 吞掉拖拽结束后 80ms 内的误触
- 顶栏「ZCode控制」复制 `ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=wss://<host>/remote/ws`
- 工作区链接 `folderHref`：`{应用url去尾斜杠}/?folder=<path>`（仅自定义 vsc 应用）

### 3.6 内置端点（`/__` 前缀）

| 端点 | 认证 | 说明 |
|---|---|---|
| `/__login` | 公开 | 登录页（GET）/ 处理登录表单（POST） |
| `/__logout` | 公开 | 清除 Cookie，返回 JSON |
| `/__version` | 需认证 | 返回 service 应用版本（`KVS_SVC_<NAME>_VERSION`；纯文本，无版本返回 `0.0.0`；`?svc=<name>` 选择指定服务，缺省=表内第一个 svc:// 服务） |
| `/__open_tkn` | 需认证 | 签发临时访问令牌（纯文本）。`GenerateTokenValue(login_token, token_timeout)`：`login_timeout=0` 时返回 `login_token` 明文；`>0` 时返回以当前时间为 ts 的 `<hash>.<ts>.<salt>`，有效期 `token_timeout` 秒（默认 300）。令牌可通过 `?<query_token_key>=<令牌>` 或请求头 `x-request-<cookie_token_key>` 使用，供 curl/脚本在 Cookie 不可用时访问受保护接口 |
| `/__restart` | 需认证 | 杀后端子进程并重置服务状态，下次请求重新触发部署流程：`/__restart` 清除版本覆盖；`/__restart?v=<版本>` 覆盖版本重启（优先于 `version`/`version_latest_url`）；`/__restart?svc=<name>` 选择指定服务（缺省=主服务）。仅 kvs 托管的 service 后端有效 |
| `/__stats` | 需认证 | 运行计数 JSON：WS 隧道（splice/generic、字节数、回退次数）、中继帧、缓存命中、goroutine/堆；`KVS_PPROF` 另行开启 pprof |
| `/__logout.vsc.js` | 公开 | 退出按钮 + Update 菜单注入脚本 |
| `/favicon.ico` | 公开 | 内联 SVG 图标 |

「需认证」仅在 `login_token` 非空时被拦截，否则直接放行。

Update 菜单注入（vscode.go + logout.vsc.js）：`login_authz = true` 且代理页面被识别为 VS Code Server 时注入两处 UI——活动栏退出按钮 `Logout`（点击 `fetch /__logout` 清 Cookie 后刷新）；Help 菜单「Update」项（位于 About 之后；自定义模态框——VS Code Web 不支持原生 `window.prompt`，颜色实时读取当前主题变量，跟随深浅色切换）：展示当前版本（取自 `/__version`），输入框默认空=保持当前版本，输入版本号后跳转 `/__restart?v=<版本>` 完成升级/重启。

## 4. 开发与验证

- Go 改动：`gofmt -l pkg` → `go vet ./...` → `go build -o kvs .`
- 内嵌 HTML/JS 改动：**无需改 Go**。`KVS_DEBUG=1` 时 `MustAsset` 从进程 CWD 读文件——`cd pkg` 后启动即热读，否则须重编译；页面 JS 可抽 `<script>` 块过 `node --check`
- 联调模板（全程用自有端口与临时 HOME）：
  ```bash
  (cd pkg && KVS_DEBUG=1 KVS_PORT=7189 KVS_HOME=../temp/.vsc-test \
    KVS_LOGIN_TOKEN=123456 KVS_ZCODEX_NODE=/bin/false ../kvs -n :zcodex)
  # curl 鉴权：-H 'Cookie: kvs=123456'
  ```
- **只 kill 自己启动的 PID**（记录 pid 文件；测试完删除临时 KVS_HOME）。生产实例（如 7080/7088/7090）属 owner，勿动、勿 pkill 按模式匹配
- 注释/日志/文档使用中文；改动行为必须同步 readme.md 与本文件对应小节

## 5. 开发路线图

已落地能力：多协议路由（http/ws/unix/file/text/svc/relay/api）、Cookie 双阶段认证、多路由表与多托管服务（svc:// + KVS_SVC_<NAME>_* 反推）、service 自动部署与 `/__restart` 版本覆盖、VS Code 专有（Update 注入/退出按钮/web-extension 本地化/语言包）、`/__cache/` 含 cache_sed、S3 mirror/syncto、zcodex 应用中心（拖拽排序/标记/备注/工具栏/ZCode控制复制/深浅色/移动适配）。

本轮已落地（性能）：
- WS 升级快路径（裸 conn + splice + 握手期缓冲字节补齐 + 可选空闲超时）与 `ws_proxy` 开关
- 中继帧路径（writev/读缓冲复用/粗粒度写超时/字长化 unmask/data 帧快路径）
- 反代 Transport 调优（空闲连接、透传 Accept-Encoding、FlushInterval）与 `ws`/`wss` scheme 映射修复
- 观测：`/__stats` 计数 + `KVS_PPROF`；基准与回归脚本在 `temp/bench/`（含 ws echo 后端、Node 基准、中继冒烟）

本轮自审修正（复查时发现并修掉）：
- 空闲超时下的写超时曾按"判活续期"处理 → 改为致命断开（否则静默截断，见 3.2.1）
- `drainBuffered` 半截写出后曾继续跑隧道 → 改为报错并放弃该隧道（帧序已错位）
- 响应体拷贝加 `httputil.BufferPool`：1MiB 响应 47.5KB/op → 15.7KB/op（`pkg/transport_test.go`）
- 空闲模式的吞吐/splice 复核：chunk=128KB/1MB/8MB 与默认 `io.Copy` 同为 ~275MiB/s，
  且 rchar≈100B/64MiB（仍命中 splice，分块没有代价）

近期方向（消解已知限制）：
- `command` 不支持带空格参数 → argv 解析或列表形式
- 外部代理缓存无大小上限 → 容量配额/LRU 淘汰
- cc~ 缓存命中 sendfile 直出、元数据内存缓存、MISS 状态码修复、`/__stats` cache 计数接线：
  **本轮已落地**（`temp/audit/ccserve` 实测 readfile 1016MiB/s → sendfile 2759MiB/s、零堆分配）
- 大上传：`read_timeout` 配置键已落地（0=不限，默认 60 保持旧行为）
- `ws~/=ws://...`（前缀 `/`）遮蔽后续路由：已加启动期 WARNING
- 扩展点索引：新后端协议 = `CreateBackendHandler` 分发 + （进程内实现时）`registerAPI`；manager 新条目类型 = `mgrBuildEntries` + 前端 `iconOf`/`TAGS`；新内置端点 = main.go mux 挂 `/__` 前缀（自动免鉴权）；新预设 = config.go zcodex 注入处模式
