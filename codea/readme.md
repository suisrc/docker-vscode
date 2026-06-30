# Codea — 反向代理网关

轻量级 Go 反向代理，面向 VS Code Server / code-server 等 Web IDE 场景。在浏览器与后端之间提供：**Token 认证**、**多后端路由**、**服务自动部署**、**外部资源代理（带缓存）**、**退出按钮注入**。

仅依赖 Go 标准库 + `embed`，无第三方依赖。

---

## 架构

```
浏览器 ──→ Codea (HTTP / HTTPS) ──→ 后端服务
              │
              ├── /__login            登录页（POST 设置 Token Cookie）
              ├── /__logout           退出（清除 Cookie）
              ├── /__logout.vsc.js    VS Code 退出按钮脚本
              ├── /__proxy/...        外部资源代理（需 SERVICE_PXY）
              └── /                   按 BACKEND_URL 路由到后端
```

---

## 构建

```bash
make build   # → ./codea
make clean
```

要求 Go 1.25+。

---

## 配置

环境变量与等价 flag（flag 优先级高于环境变量）：

| 变量 / flag | 默认 | 说明 |
|---|---|---|
| `BACKEND_URL` / `-backend` | **必填** | 后端地址，语法见下节 |
| `SERVICE_WSC` / `-wsc` | 空 | 服务工作目录（其他 SERVICE_* 路径的基准） |
| `SERVICE_URL` / `-svc-url` | 空 | VS Code 服务端下载地址 |
| `SERVICE_VER` / `-svc-ver` | 空 | 下载缓存文件路径，`{ext}` 运行时解析 |
| `SERVICE_DIR` / `-svc-dir` | 空 | 解压安装目录 |
| `SERVICE_FIX` / `-svc-fix` | 空 | 启动前脚本（`file://` 走 shebang；否则 `sh -c`） |
| `SERVICE_CMD` / `-service` | 空 | 以子进程启动的后端命令 |
| `SERVICE_PXY` / `-svc-pxy` | 空 | `/__proxy/` 缓存根目录；**留空则禁用 `/__proxy/`** |
| `VSCODE_PORT`/`PROXY_PORT` / `-port` | `7080` | 监听端口（SSL 时 HTTP→port, HTTPS→port+1） |
| `TOKEN_COOKIE` / `-cookie` | `vscode-tkn` | Token Cookie 名 |
| `PROXY_USE_SSL` / `-use-ssl` | `false` | 启用 HTTPS（自签名 ECDSA 证书，10 年） |
| `PROXY_HEADER_*` | 空 | 请求头改写，见「请求头改写」 |

---

## 后端配置 (`BACKEND_URL`)

### 单后端

```
BACKEND_URL=http://localhost:8080
BACKEND_URL=unix:///run/code-server.sock
BACKEND_URL=file:///var/www/html
BACKEND_URL=text://Hello World
```

| Scheme | 行为 |
|---|---|
| `http` / `https` | HTTP 反向代理 |
| `unix` | Unix socket 反向代理 |
| `file` | 静态文件服务器 |
| `text` | 返回纯文本字面量 |

单后端时自动视为服务后端（受 Codea 托管）。

### 多后端（路由前缀）

`;` 分隔多个 `prefix=url` 段；前缀加 `^` 标记为 Codea 托管的服务后端：

```
BACKEND_URL=/api/=http://api:8080;/cdn/=file:///var/www;/x/=text://ok
BACKEND_URL=^/vscode/=http://code-server:8080;/files/=file:///srv/data
```

`/api/v1/foo` → `http://api:8080/v1/foo`；`/cdn/img/logo.png` → `/var/www/img/logo.png`。

---

## Token 认证

1. 后端返回 401/403 → Codea 拦截并返回 `login.html`。
2. 用户提交 Token → 写入 `HttpOnly` + `SameSite=Lax` Cookie，重定向回原页。
3. 后续请求携带 Cookie，由后端自行校验。
4. 退出：访问 `/__logout` 清除 Cookie；VS Code 工具栏注入的退出按钮同样跳转至此。

---

## 请求头改写 (`PROXY_HEADER_*`)

```bash
PROXY_HEADER_X-Forwarded-Proto=https   # 设置/覆盖
PROXY_HEADER_X-Unwanted-Header=         # 删除（值为空）
```

---

## 服务自动部署

设置 `SERVICE_URL` 后，Codea 在启动 `SERVICE_CMD` 前自动执行：

1. `SERVICE_DIR` 已有有效安装（`bin/code-server` 存在）→ 跳过。
2. 跟随 `SERVICE_URL` 重定向，解析扩展名 → 替换 `SERVICE_VER` 中的 `{ext}`。
3. `SERVICE_VER` 缓存未命中 → 下载（原子 temp+rename）。
4. 解压 tar.gz → `SERVICE_DIR`（剥离顶层目录）。
5. 执行 `SERVICE_FIX`。

准备期间所有请求返回加载页（每 2 秒自动刷新），完成后恢复正常。

```bash
export VSCODE_HASH="7e7950df89d055b5a378379db9ee14290772148a"
export SERVICE_WSC="/path/to/workspace"
export SERVICE_URL="https://update.code.visualstudio.com/commit:${VSCODE_HASH}/server-linux-x64-web/stable"
export SERVICE_VER="${SERVICE_WSC}/.vcache/version/${VSCODE_HASH}.{ext}"
export SERVICE_PXY="${SERVICE_WSC}/.vcache/proxies/"
export SERVICE_DIR="${SERVICE_WSC}/.vserve/${VSCODE_HASH}/"
export SERVICE_FIX="sed -i 's|https://www.vscode-unpkg.net/nls/|/__proxy/www.vscode-unpkg.net/nls/|g' ${SERVICE_DIR}/product.json"
export SERVICE_CMD="${SERVICE_DIR}/bin/code-server --accept-server-license-terms \
    --socket-path /var/run/vscode.sock \
    --server-data-dir ${SERVICE_WSC}/.vscode \
    --connection-token 7788"
./codea --backend unix:///var/run/vscode.sock
```

---

## 外部资源代理 (`/__proxy/`)

仅当配置 `SERVICE_PXY` 时启用。URL 格式：

```
/__proxy/[cc~]{scheme}:{host}[/path][?query]
```

| 示例 | 行为 |
|---|---|
| `/__proxy/cdn.example.com/lib.js` | 代理 `https://cdn.example.com/lib.js`（默认 HTTPS） |
| `/__proxy/https:cdn.example.com/lib.js` | 显式 HTTPS |
| `/__proxy/cc~cdn.example.com/lib.js` | 代理并缓存（仅 GET 且 2xx） |
| `/__proxy/http:example.com/api` | 代理 HTTP |

**缓存布局**（直接映射 URL 路径）：

```
{SERVICE_PXY}/{scheme}:{host}/path/to/file.js       → 正文
{SERVICE_PXY}/{scheme}:{host}/path/to/file.js_.json  → 元数据（状态码 + 精选响应头）
```

- 根路径 `/` 存为 `__index`。
- 仅缓存 2xx 的 GET 响应；`X-Cache: HIT`/`MISS` 标识命中。
- 独立 Transport，`ResponseHeaderTimeout=30s`，超时 5 分钟。

---

## 退出按钮注入

代理的 HTML 顶层导航响应若命中应用指纹，在 `</body>` 前注入对应脚本：

- VS Code workbench（指纹 `<meta id="vscode-workbench-web-configuration"`）→ 注入 `/__logout.vsc.js`。

扩展其他应用：在 `appDetectors` 中新增「指纹 + 脚本标签」即可。

---

## HTTPS / TLS

`PROXY_USE_SSL=1`：生成 ECDSA P-256 自签名证书（CN `CodeAuth`，10 年，TLS 1.2+）。HTTP 端口 = `PROXY_PORT`，HTTPS = `PROXY_PORT + 1`。

---

## 子进程管理 (`SERVICE_CMD`)

- 独立进程组（`Setpgid`），stdout/stderr 继承。
- 收到 `SIGINT`/`SIGTERM`：停止接收新连接 → 15 秒优雅排空 → 向进程组发 `SIGTERM` → 清理 Unix socket。
- 命令按空白分割，**不支持带空格的参数**。

---

## 安全要点

- Token Cookie：`HttpOnly` + `SameSite=Lax`。
- 登录重定向仅允许同源相对路径（防开放重定向）。
- 外部代理仅允许 `http`/`https`（防 SSRF）。
- 自签名证书仅用于传输加密，不提供身份信任。

---

## 开发与测试

```sh
# 纯文本代理
BACKEND_URL="text://hello world" ./codea
curl http://127.0.0.1:7080

# HTTP 后端
code-server --host 0.0.0.0 --port 6802 --connection-token 77885566
BACKEND_URL=http://127.0.0.1:6802 ./codea

# Unix socket 后端 + HTTPS
rm /var/run/vscode.sock && code-server --socket-path /var/run/vscode.sock --connection-token 77885566
BACKEND_URL=unix:///var/run/vscode.sock PROXY_USE_SSL=1 PROXY_PORT=7080 ./codea

# 多后端 + 子进程一条命令启动
export PROXY_HEADER_x-forwarded-port='443'
# export VSCODE_HASH="7e7950df89d055b5a378379db9ee14290772148a"
export VSCODE_HASH="vscode:latest"
export SERVICE_FIX='sed -i "s|https://www.vscode-unpkg.net/nls/|/__proxy/cc~http:127.0.0.1/nls/|g" ${SERVICE_DIR}/product.json'
export SERVICE_CMD='${SERVICE_DIR}/bin/code-server --accept-server-license-terms \
    --socket-path /var/run/vscode.sock \
    --server-data-dir ${SERVICE_WSC}/.vscode \
    --connection-token 7788'
SERVICE_WSC=/wsc/go/github/ws01/docker-vscode/temp ./codea --use-ssl --backend "/test/=text://test;^/=unix:///var/run/vscode.sock"

# 缓存代理（需 SERVICE_PXY）
curl http://127.0.0.1:7080/__proxy/cc~example.com/index.html
```
