# Kin — 反向代理网关

轻量级 Go 反向代理，面向 VS Code Server / code-server 等 Web IDE 场景。提供 **Token 认证**、**多后端路由**、**服务自动部署**、**外部资源代理（带缓存）**、**退出按钮注入**。

仅依赖 Go 标准库 + `embed`，无第三方依赖。

---

## 快速开始

```bash
make build
BACKEND_URL=http://localhost:8080 ./kin
```

---

## 架构

```
浏览器 ──→ Kin (HTTP/HTTPS) ──→ 后端服务
              │
              ├── /__login            登录页（POST 设置 Token Cookie）
              ├── /__logout           退出（清除 Cookie）
              ├── /__logout.vsc.js    VS Code 退出按钮脚本
              ├── /__proxy/...        外部资源代理（需 SERVICE_PXY）
              └── /                   按 BACKEND_URL 路由到后端
```

---

## 配置

环境变量优先于 flag（flag 仅在环境变量未设置时生效）：

| 变量 | flag | 默认 | 说明 |
|---|---|---|---|
| `BACKEND_URL` | `-backend` | **必填** | 后端地址，见「后端语法」 |
| `SERVICE_WSC` | `-svc-wsc` | 空 | 工作目录基准 |
| `SERVICE_URL` | `-svc-url` | 空 | 后端下载地址 |
| `SERVICE_VER` | `-svc-ver` | 空 | 缓存路径，支持 `{ext}` |
| `SERVICE_DIR` | `-svc-dir` | 空 | 解压目录 |
| `SERVICE_PRE` | `-svc-pre` | 空 | 启动前脚本（`file://` 走 shebang；否则 `sh -c`） |
| `SERVICE_CMD` | `-svc-cmd` | 空 | 以子进程启动的后端命令 |
| `SERVICE_PXY` | `-svc-pxy` | 空 | `/__proxy/` 缓存根；留空禁用 |
| `VSCODE_PORT` / `PROXY_PORT` | `-port` | `7080` | 监听端口（SSL 时 HTTPS = port+1） |
| `TOKEN_COOKIE` | `-cookie` | `vscode-tkn` | Token Cookie 名 |
| `PROXY_USE_SSL` | `-use-ssl` | `false` | 启用自签名 HTTPS |
| `PROXY_HEADER_*` | — | — | 请求头改写（`Xxx=Val` 设置；`Xxx=` 删除） |

---

## 后端语法 (`BACKEND_URL`)

**单后端**（自动视为 Kin 托管的服务后端）：

```
http://localhost:8080
unix:///run/code-server.sock
file:///var/www/html
text://Hello World
```

**多后端**（`;` 分隔，`^` 前缀标记服务后端）：

```
/api/=http://api:8080;/cdn/=file:///var/www
^/vscode/=http://code-server:8080;/files/=file:///srv/data
```

---

## 服务自动部署

设置 `SERVICE_URL` 后，首次请求触发：

1. `SERVICE_DIR` 非空 → 跳过
2. 跟随重定向解析扩展名 → 替换 `{ext}`
3. 下载缓存（原子 temp+rename）
4. 解压 tar.gz（剥离顶层目录）
5. 执行 `SERVICE_PRE`
6. 启动 `SERVICE_CMD`

准备期间返回加载页（503 + 自动刷新），完成后正常代理。

```bash
export VSCODE_HASH="7e7950df89d055b5a378379db9ee14290772148a"
export SERVICE_WSC="/path/to/workspace"
export SERVICE_URL="https://update.code.visualstudio.com/commit:${VSCODE_HASH}/server-linux-x64-web/stable"
export SERVICE_VER="${SERVICE_WSC}/.vcache/version/${VSCODE_HASH}.{ext}"
export SERVICE_PXY="${SERVICE_WSC}/.vcache/proxies/"
export SERVICE_DIR="${SERVICE_WSC}/.vserve/${VSCODE_HASH}/"
export SERVICE_PRE="sed -i 's|https://www.vscode-unpkg.net/nls/|/__proxy/www.vscode-unpkg.net/nls/|g' ${SERVICE_DIR}/product.json"
export SERVICE_CMD="${SERVICE_DIR}/bin/code-server --accept-server-license-terms --socket-path /var/run/vscode.sock --connection-token 7788"
./kin --backend unix:///var/run/vscode.sock
```

---

## Token 认证

- 后端返回 **401** → 拦截并返回 `login.html`
- 提交 Token → 写入 `HttpOnly` + `SameSite=Lax` Cookie
- `/__logout` 清除 Cookie
- VS Code 工具栏退出按钮通过注入脚本跳转至此

---

## 外部资源代理 (`/__proxy/`)

```
/__proxy/[cc~]{scheme}:{host}[/path][?query]
```

- `cc~` 前缀 = 缓存（仅 GET 2xx）
- 默认 HTTPS；`http:` 显式指定
- 缓存布局：`{SERVICE_PXY}/{scheme}:{host}/path` + `_.json` 元数据
- `X-Cache: HIT/MISS` 标识命中

---

## 退出按钮注入

代理的 HTML 顶层导航响应若命中指纹，在 `</body>` 前注入脚本：

- VS Code: `<meta id="vscode-workbench-web-configuration"` → `/__logout.vsc.js`

扩展：在 `appDetectors` 追加「指纹 + 脚本标签」。

---

## HTTPS / TLS

`PROXY_USE_SSL=1`：ECDSA P-256 自签名证书（CN `CodeAuth`，10 年，TLS 1.2+）。

---

## 子进程管理

- 独立进程组（`Setpgid`），`SIGINT`/`SIGTERM` → 15s 优雅排空 → `SIGTERM` 进程组 → 清理 Unix socket
- 命令按空白分割，**不支持带空格的参数**

---

## 安全

- Cookie: `HttpOnly` + `SameSite=Lax`
- 登录重定向仅允许同源相对路径
- 外部代理仅允许 `http`/`https`（防 SSRF）
- 自签名证书仅加密，无身份信任

---

## 已知限制

- `SERVICE_CMD` 不支持带空格的参数
- 403 当前与 401 同样重定向到登录页（语义待修正）
- 外部代理缓存无大小限制
- `VSCODE_HASH=vscode:latest` 在网络不可达时会导致启动失败
curl http://127.0.0.1:7080

# HTTP 后端
code-server --host 0.0.0.0 --port 6802 --connection-token 77885566
BACKEND_URL=http://127.0.0.1:6802 ./kin

# Unix socket 后端 + HTTPS
rm /var/run/vscode.sock && code-server --socket-path /var/run/vscode.sock --connection-token 77885566
BACKEND_URL=unix:///var/run/vscode.sock PROXY_USE_SSL=1 PROXY_PORT=7080 ./kin

# 多后端 + 子进程一条命令启动
export PROXY_HEADER_x-forwarded-port='443'
# export VSCODE_HASH="7e7950df89d055b5a378379db9ee14290772148a"
export VSCODE_HASH="vscode:latest"

export SERVICE_CMD='${SERVICE_DIR}/bin/code-server --accept-server-license-terms \
    --socket-path /var/run/vscode.sock \
    --server-data-dir ${SERVICE_WSC}/.vsc \
    --connection-token 7788'
SERVICE_WSC=/wsc/go/github/ws01/docker-vscode/temp ./kin --use-ssl --backend "/test/=text://test;^/=unix:///var/run/vscode.sock"

# 缓存代理（需 SERVICE_PXY）
curl http://127.0.0.1:7080/__proxy/cc~example.com/index.html
```
