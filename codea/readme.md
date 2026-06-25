# Codea — 反向代理网关

Codea 是一个轻量级 Go 反向代理，专为 VS Code Server / code-server 等 Web IDE 场景设计。它在前端与后端服务之间提供 **Token 认证**、**多后端路由**、**VS Code 更新缓存代理**、**外部代理（带缓存）**、**CORS 正文重写** 以及 **退出按钮注入** 等功能。

---

## 1. 架构概览

```
浏览器 ──→ Codea (HTTP :7080 / HTTPS :7081) ──→ 后端服务
                │
                ├── /__login        登录页
                ├── /__logout       退出（清除 Cookie）
                ├── /__logout.vsc.js VS Code 退出按钮脚本
                ├── /__vscode/      VS Code 更新 API 代理（带缓存）
                ├── /__proxy/       外部代理（可选缓存）
                └── /               根据 BACKEND_URL 路由到后端
```

---

## 2. 快速构建

```bash
cd codea
make build    # → 输出可执行文件 codea
make clean    # → 清理产物
```

依赖：Go 1.25+，无外部依赖（仅标准库 + `embed` 静态资源）。

---

## 3. 环境变量 & 启动参数

| 变量 / flag | 默认值 | 说明 |
|---|---|---|
| `BACKEND_URL` / `-backend` | **必填** | 后端服务地址，支持单/多后端，详见 §4 |
| `SERVICE_CMD` / `-service` | 空 | 以子进程方式启动的后端命令 |
| `VSC_PORT` / `-port` | `7080` | 代理监听端口（SSL 启用时 HTTP→7080, HTTPS→7081） |
| `TOKEN_COOKIE` / `-cookie` | `vscode-tkn` | Token 认证 Cookie 名 |
| `PROXY_USE_SSL` / `-ssl` | `false` | 启用 HTTPS（自签名证书，10 年有效） |
| `VSC_CACHE` | `/app/.vscode` | 缓存根目录 |
| `VSC_CORS_IDX` | 空 | 索引页 CORS 正文替换规则，见 §8 |
| `VSC_CORS_SUF_*` | 空 | 按路径后缀匹配的 CORS 替换规则 |
| `VSC_CORS_PRE_*` | 空 | 按路径前缀匹配的 CORS 替换规则 |
| `PROXY_HEADER_*` | 空 | 代理请求头改写，见 §6 |

---

## 4. 后端配置语法 (`BACKEND_URL`)

### 4.1 单后端

```
BACKEND_URL=http://localhost:8080
BACKEND_URL=unix:///run/code-server.sock
BACKEND_URL=file:///var/www/html
BACKEND_URL=text://Hello World
```

| Scheme | 行为 |
|---|---|
| `http://` / `https://` | 标准 HTTP 反向代理 |
| `unix://` | Unix Domain Socket 反向代理 |
| `file://` | 静态文件服务器（目录） |
| `text://` | 返回纯文本字面量 |

### 4.2 多后端（路由前缀）

用 `;` 分隔多个 prefix=url 段，前缀以 `/` 开头：

```
BACKEND_URL=/api/=http://api:8080;/cdn/=file:///var/www;/x/=text://ok
```

请求路径 `/api/v1/foo` → 代理到 `http://api:8080/v1/foo`；
`/cdn/img/logo.png` → 读取 `/var/www/img/logo.png`。

> 单段形式 `/=unix:///path` 也被正确识别为多后端格式（根前缀 `/`）。

---

## 5. Token 认证流程

1. **未认证请求** → 后端返回 401/403 → Codea 拦截并展示 `login.html` 登录页。
2. **用户提交 Token** → Codea 设置 `vscode-tkn` Cookie（`HttpOnly` + `SameSite=Lax`），重定向回原始页面。
3. **后续请求** → Cookie 自动携带，后端自行校验 Token。
4. **退出** → 访问 `/__logout`，清除 Cookie；VS Code 工具栏中注入的退出按钮也会跳转到 `/__logout`。

---

## 6. 请求头改写 (`PROXY_HEADER_*`)

在转发请求到后端时，可设置或删除特定请求头：

```bash
# 设置/覆盖
PROXY_HEADER_X-Forwarded-Proto=https
PROXY_HEADER_X-Custom-Header=myvalue

# 删除
PROXY_HEADER_X-Unwanted-Header=
```

---

## 7. VS Code 更新代理 (`/__vscode/`)

代理 VS Code 官方更新 API (`https://update.code.visualstudio.com`)，**本地缓存** 以加速内网部署。

### 端点

| 路由 | 行为 |
|---|---|
| `GET /__vscode/api/latest/{platform}/{quality}` | 获取最新版本 JSON（缓存到 `{cache}/api/latest/`） |
| `GET /__vscode/commit:{commit}/{platform}/{quality}` | 下载指定 commit 的归档（跟随上游重定向，缓存大文件） |
| `GET /__vscode/download/{quality}/{commit}/vscode-{platform}.{ext}` | 以正确文件名提供缓存文件下载 |

### 缓存策略

- **latest API**：命中返回 `X-Cache: HIT`；未命中从上游拉取 → 写缓存 → 返回。
- **commit 下载**：先查本地缓存；未命中从上游获取（跟随所有重定向）→ 原子写入缓存 → 重定向到 `/__vscode/download/` 提供下载（带正确的 `Content-Disposition` 文件名）。
- 大文件采用 **流式落盘**（`streamToAtomicFile`），避免将数百 MB 的归档读入内存。
- 缓存写入使用 **temp + rename** 策略，防止崩溃导致损坏。

---

## 8. CORS 正文重写 (`VSC_CORS_*`)

用于 VS Code 私有部署场景，将响应正文中的外部 URL 替换为内网地址。

### 规则类型

```bash
# 索引页（/ 或 /index.html）替换
VSC_CORS_IDX=https://vscode.example.com->https://internal.local,old->new

# 按路径后缀匹配（. - / 统一替换为 _，小写）
VSC_CORS_SUF_workbench_js=https://cdn->https://internal-cdn

# 按路径前缀匹配
VSC_CORS_PRE_extensions=https://marketplace->https://private-marketplace
```

- 只对 `.js`、`.html`、`.json` 及索引页生效。
- 支持多对 `from->to`，逗号分隔。
- 路径规范化：`.` `-` `/` → `_`，全部小写。

---

## 9. 外部代理 (`/__proxy/`)

通用 HTTP/HTTPS 外部资源代理，支持可选缓存。

### URL 格式

```
/__proxy/[cc+]{scheme}:{host}[/path][?query]
```

| 示例 | 行为 |
|---|---|
| `/__proxy/cdn.example.com/lib.js` | 代理 `https://cdn.example.com/lib.js`（默认 HTTPS） |
| `/__proxy/https:cdn.example.com/lib.js` | 显式 HTTPS |
| `/__proxy/cc+cdn.example.com/lib.js` | 代理并缓存（仅 GET 且 2xx） |
| `/__proxy/http:example.com/api` | 代理 HTTP |

### 缓存细节

- 缓存目录结构直接映射 URL 路径：
  ```
  {VSC_CACHE}/proxy/{scheme}:{host}/path/to/file.js       → 正文
  {VSC_CACHE}/proxy/{scheme}:{host}/path/to/file.js.meta  → 元数据
  ```
- 根路径 `/` 映射为 `__index`（即 `{root}/https:example.com/__index`）
- `.meta` 文件：JSON 格式，保存状态码和精选响应头
- 正文文件：流式写入，无扩展名改名
- `X-Cache: HIT` / `X-Cache: MISS` 响应头标识命中状态
- 只缓存 2xx 响应；非 GET 请求不写缓存
- 外部代理使用独立 Transport，`ResponseHeaderTimeout=30s`

---

## 10. VS Code 退出按钮注入

当代理的 HTML 响应包含 VS Code workbench 指纹 (`<meta id="vscode-workbench-web-configuration"`) 时，自动在 `</body>` 前注入：

```html
<script src="/__logout.vsc.js"></script>
```

`logout.vsc.js` 在 VS Code 活动栏工具栏注入一个退出图标按钮，点击跳转到 `/__logout`。

> 扩展其他应用：在 `appDetectors` 列表中添加新的指纹 + 脚本标签即可。

---

## 11. HTTPS / TLS

启用 `PROXY_USE_SSL=1` 时：

- 生成 **ECDSA P-256** 自签名证书
- Common Name: `CodeAuth`，有效期 10 年
- HTTP 端口 = `VSC_PORT`，HTTPS 端口 = `VSC_PORT + 1`
- 仅启用 TLS 1.2+

---

## 12. 后端子进程管理 (`SERVICE_CMD`)

设置 `SERVICE_CMD` 后，Codea 在启动时以子进程方式启动后端命令：

```bash
SERVICE_CMD="code-server --bind-addr 0.0.0.0:8080"
```

- 子进程创建独立进程组（`Setpgid=true`）
- 标准输出/错误继承到 Codea
- 收到 `SIGINT`/`SIGTERM` 时：
  1. 停止接受新连接
  2. 等待 15 秒让活跃连接完成
  3. 向子进程组发送 `SIGTERM`
  4. 清理 Unix socket 文件

---

## 13. 静态资源 (embed)

编译时嵌入以下文件：

| 文件 | 用途 |
|---|---|
| `login.html` | 登录页面（支持暗/亮主题，中文本地化） |
| `logout.html` | 退出确认页面 |
| `logout.vsc.js` | VS Code 工具栏退出按钮注入脚本 |

---

## 14. 安全设计要点

- Token Cookie 设置 `HttpOnly` + `SameSite=Lax`
- 登录重定向仅允许相对路径（`safeReferer`）
- 外部代理限制 `http`/`https` scheme（防 SSRF）
- 自签名证书仅用于传输加密，不提供身份信任
- 优雅关闭避免连接中断

---

## 15. 典型使用场景

### Docker / K8s Sidecar

```dockerfile
COPY codea /usr/local/bin/
ENV BACKEND_URL=http://127.0.0.1:8080
ENV VSC_PORT=7080
ENV TOKEN_COOKIE=my-token
CMD ["codea"]
```

### 配合 code-server

```bash
export BACKEND_URL=http://127.0.0.1:8080
export SERVICE_CMD="code-server --bind-addr 127.0.0.1:8080 --auth none"
export VSC_CORS_IDX=https://update.code.visualstudio.com->/__vscode
./codea
```

### 多服务路由

```bash
export BACKEND_URL="/vscode/=http://code-server:8080;/files/=file:///srv/data"
./codea
```
