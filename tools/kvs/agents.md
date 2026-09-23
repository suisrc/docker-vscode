# agents.md — kvs 开发文档（AI 向）

面向 AI 协作开发的内部机制与约定速查。用户文档（简介/快速开始/使用/感谢）在 [readme.md](readme.md)；**各配置键的完整说明以 demo 模板注释为准**（`kvs demo` 生成，即 `pkg/kvs.default.ini` / `pkg/kvs.vscode.ini`）。分工：readme=使用说明，demo 模板=配置参考，本文件=架构与开发。两文档与代码冲突时以代码为准。

## 1. 定位与硬约束

- 单二进制 Go 反向代理：**仅标准库 + `embed`，禁止引入第三方依赖**
- 行为全部由 kvs.ini（ini + 值模板展开）驱动；启动 `-c` 必填
- 交付形态：`make build` → `./kvs`，静态资源全部内嵌
- 生产实例归 owner 所有：**不得 kill/重启非自己启动的进程**；部署与升级只由 owner 决定

## 2. 代码地图

| 文件 | 职责 |
|---|---|
| `main.go` | CLI 分发（`help`/`demo`/`mirror`/`syncto` 子命令；无子命令=代理模式）、mux 装配、登录/内置端点、注入脚本服务 |
| `pkg/config.go` | ini 解析 + 值模板展开、SVC_* 内部变量、版本覆盖（`GOverrideVersion`）、zcodex 预设注入（KVS_PROXIES 拼装 `wsws://`/`api://manager`） |
| `pkg/backend.go` | `Backend` 抽象；`CreateBackendHandler` 按 scheme 分发；`registerAPI`（api:// 进程内注册表）；ws 代理；`precompressedFileServer`（br/gz/zst 预压缩静态） |
| `pkg/service.go` | service 生命周期：检测→版本解析→下载→解压→脚本→启停 |
| `pkg/cache.go` | `/__cache/` 外部资源代理；`cc~` 缓存；cache_sed once/each/none 与 `_.json` 元数据 |
| `pkg/server.go` | 服务器构建（HTTP + HTTPS=port+1 自签 ECDSA P-256）、静态资产/登录页服务、`isBrowserRequest` |
| `pkg/vscode.go` | VS Code 专有：web-extension-resource 本地化（上游乱码 issue microsoft/vscode#299425）、语言包/汉化链路 |
| `pkg/vagent.go` | agent 端点：`vsc_agent_cmds` 预设表（JSON）、端点目录扫描（*.json） |
| `pkg/zcode.go` | `wsws://` 进程内中继（设备注册/心跳/远程控制）；`zcodeStore`（zcodex.json 持久化 + 旧版迁移 + 防抖原子落盘）；`KVS_ZCODEX_NODE` 懒启动 |
| `pkg/manager.go` | 应用中心：合并视图组装、REST API、在线探测（45s 缓存）、`registerAPI("manager")` |
| `pkg/manager.html` | 管理页单文件前端（客户端渲染，PC 卡片/移动行双布局） |
| `pkg/wsconn.go` | 最小 RFC 6455 WebSocket 服务端（stdlib 实现） |
| `pkg/proc.go` | 子进程 Setsid 脱离、终止时前台进程组恢复 |
| `pkg/s3mirror.go` | `mirror`/`syncto` 的 S3 同步实现（owner 开发中，勿擅自重构） |
| `pkg/help.go` | help 文本——新增配置项/环境变量必须同步 |
| `pkg/` 资产 | `manager.html` `login.html` `loading.html` `logout.vsc.js` `kvs.default.ini` `kvs.vscode.ini` `favicon.ico`，`go:embed` 进二进制 |

## 3. 核心机制

### 3.1 值模板展开（config.go）

`{VAR}`、`{VAR:-default}`、`${VAR}`/`$VAR`（os.ExpandEnv）、`@now`（仅 text 后端，RFC3339）。`[service]` 值可引用 SVC_* 内部变量，加载序：`SVC_HOME`→`SVC_VERSION_BASE_URL`→`SVC_VERSION`→`SVC_VERSION_HASH`（留空=SVC_VERSION）→`SVC_PACKAGE_EXT`→`SVC_BIN_HOME`。

### 3.2 路由与后端协议

`[proxies]` 按文件顺序匹配。前缀标记：普通前缀；`&`（托管服务，参与启停与 loading 页，**必须显式标记**）；`^`（正则）；`http(s)://`、`ws(s)://` 开头（全域名）。

后端 scheme 全集（readme 仅列用户常用项）：

| scheme | 实现 |
|---|---|
| `http`/`https` | 反向代理（httputil.ReverseProxy） |
| `ws`/`wss` | 同 http（升级请求透传） |
| `unix` | Unix domain socket |
| `file` | 静态文件（含 br/gz/zst 预压缩优选） |
| `text` | 直接返回文本，支持 `@now` |
| `wsws://<name>` | 进程内 zcode 中继（zcode.go），如 `wsws://zcode-clients` |
| `api://<name>` | 进程内 handler 注册表（`registerAPI`），目前仅 `api://manager` |

service 生命周期（`&` 后端，service.go）：`check` 检测外部服务（已存在则只代理不托管启停）→ `bin_home` 非空跳过下载 → 解析下载地址（`download` 优先，否则 `download_info`+`download_field_url`）→ 跟随重定向取扩展名 `SVC_PACKAGE_EXT` → 下载到 `{cache_dir}/cache/version/{version}_{version_hash}.{ext}` → 解压到 `bin_home` → 执行 `init_shell`；`stop_shell` 仅 kvs 托管的后端退出时执行。

### 3.3 Cookie 认证（main.go / server.go）

两阶段。**请求阶段拦截**（`login_token` 非空时生效）：所有非 `/__` 前缀路径校验 Cookie，内置端点与 `/__cache/` 始终放行；常数时间比较防时序攻击。**响应阶段重定向**（需 `login_authz=true`）：后端返回 401/403 → 替换为登录页；`/__logout` 清除 Cookie。注意：`login_token` 为空时请求阶段无拦截，`login_authz` 仅在后端自身返回 401/403 时生效。`login_timeout`：

| 值 | Cookie 格式 | 校验 | 过期 |
|---|---|---|---|
| `0`/空 | `login_token` 明文 | 直接比对 | session |
| `>0` | `<hash>.<ts>.<salt>`，hash=`sha256(ts+salt+login_token)[:24]`，salt=16 位随机 hex | 哈希比对 + 过期检查 | 剩余 ≤1/4 时自动续期（滑动过期） |
| `<0` | 启动报错 | | |

### 3.4 `/__cache/`（cache.go）

- URL：`/__cache/[cc~]{scheme}:{host}[/path][?query]`；`cc~` 前缀=缓存（仅 GET 2xx）
- cache_sed 规则：`文件名(一个*)|old|new||...`，new 中 `>host<` 展开为当前请求 Host
- 模式在**首次写入缓存时**判定并持久化到元数据，之后命中只按元数据处理（不反查运行时规则）：
  - `once`：new 与请求无关 → 替换烤入存储体，原文备份 `<file>_.bak1`（仅一次），命中直接返回
  - `each`：某条替换含 `>host<` 且原文存在 old → once 规则先烤入，原始内容明文存储，each 数组存 `_.json` 的 `sed.each`；命中按当前 Host 重替换，gzip 跟随原始响应（`src_gzip`）
  - `none`：匹配文件名但内容未变 → 原样存取
  - 同一文件可同时有 once+each：once 先替换保存，文件标记 each，每请求补 each 部分
- 布局：`{cache_dir}/cache/ccproxy/{scheme}:{host}/path`（存储体）、`path_.json`（status/headers + sed）、`path_.bak1`

### 3.5 zcodex 模式与 manager（zcode.go / manager.go）

`-c zcodex` 预设注入的环境变量（用户显式设置优先）：`KVS_PATH_PUBLIC`、`KVS_SVC_CHECK_URL`（探活 `/api/server-info`，默认端口 7587）、`KVS_SVC_COMMAND`（本地 zcode 懒启动）、`KVS_PROXIES`（`/zcode` + `/=api://manager`）。

管理页 `manager.html` 挂载于 `/=api://manager`（客户端渲染，支持深浅色主题）。条目四类：`[ZCD]local` 本机 zcode（`/zcode`，kvs 懒启动，始终可点）；`[ZCD]xxx` 通过 wsws 中继注册的远程 zcode 桌面端（在线时点击打开远程控制终端）；`[VSC]zzz` 手动添加的 VS Code 应用（同一设备的不同工作区以快捷方式挂同一卡片，访问地址 `{卡片同host}/?folder=<工作区路径>`，可编辑/删除）；`[APP]`/`[LNX]` 「其他」类型设备（Logo 四选一 vsc/zcd/linux/other）。卡片顺序固定（不按访问时间排序），拖拽调整后持久化到 `order` 段，新条目追加末尾。

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
| `/__version` | 需认证 | 返回 service 应用版本（`SVC_VERSION`，含 `?v=` 覆盖值；纯文本，无版本返回 `0.0.0`） |
| `/__restart` | 需认证 | 杀后端子进程并重置服务状态，下次请求重新触发部署流程：`/__restart` 清除版本覆盖；`/__restart?v=<版本>` 写 `GOverrideVersion` 指定版本重启（优先于 `version`/`version_latest_url`）。仅 kvs 托管的 service 后端有效 |
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
    KVS_LOGIN_TOKEN=123456 KVS_ZCODEX_NODE=/bin/false ../kvs -c zcodex)
  # curl 鉴权：-H 'Cookie: kvs=123456'
  ```
- **只 kill 自己启动的 PID**（记录 pid 文件；测试完删除临时 KVS_HOME）。生产实例（如 7080/7088/7090）属 owner，勿动、勿 pkill 按模式匹配
- 注释/日志/文档使用中文；改动行为必须同步 readme.md 与本文件对应小节

## 5. 开发路线图

已落地能力：多协议路由（http/ws/unix/file/text/wsws/api）、Cookie 双阶段认证、service 自动部署与 `/__restart` 版本覆盖、VS Code 专有（Update 注入/退出按钮/web-extension 本地化/语言包）、`/__cache/` 含 cache_sed、S3 mirror/syncto、zcodex 应用中心（拖拽排序/标记/备注/工具栏/ZCode控制复制/深浅色/移动适配）。

近期方向（消解已知限制）：
- `command` 不支持带空格参数 → argv 解析或列表形式
- 外部代理缓存无大小上限 → 容量配额/LRU 淘汰
- 扩展点索引：新后端协议 = `CreateBackendHandler` 分发 + （进程内实现时）`registerAPI`；manager 新条目类型 = `mgrBuildEntries` + 前端 `iconOf`/`TAGS`；新内置端点 = main.go mux 挂 `/__` 前缀（自动免鉴权）；新预设 = config.go zcodex 注入处模式
