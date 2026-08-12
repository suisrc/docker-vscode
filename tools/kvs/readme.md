# kvs — 反向代理网关

轻量级 Go 反向代理。提供 **Cookie 认证**、**多后端路由（前缀/正则）**、**服务自动部署**、**外部资源代理（带缓存）**、**退出按钮注入**、**S3 镜像同步**。

仅依赖 Go 标准库 + `embed`，无第三方依赖。**完全通过 `kvs.ini` 配置文件驱动**。

---

## 快速开始

```bash
make build
./kvs help                          # 查看帮助
./kvs demo                          # 生成示例配置 ./kvs.ini
# 编辑 kvs.ini
./kvs -c kvs.ini                    # 指定配置文件启动
./kvs -c default                    # 使用 embed 中的 kvs.ini.example
./kvs -n "/=http://127.0.0.1:8080"  # 内联路由，自动补充 -c default
```

**`-c` 为必填项**，不指定直接报错退出。特殊值 `default` 使用 embed 中的 `kvs.ini.example`，无需磁盘文件。

`-n` 出现时自动补充 `-c default`，用内联路由替代 `[proxies]` 段，详见 [内联路由 `-n`](#内联路由--n)。

---

## 子命令

| 命令 | 说明 |
|---|---|
| `kvs help` | 显示帮助信息（也支持 `-h`、`--help`） |
| `kvs demo` | 生成示例配置文件 `kvs.ini` |
| `kvs mirror -c <config> [version]` | 同步 VS Code 版本到 S3 兼容存储 |
| `kvs mirror -c default` | 使用内置默认配置同步 latest 版本 |
| `kvs mirror -c default 1.130.0` | 同步指定版本 |

---

## 配置文件 (`kvs.ini`)

### 值模板语法

所有值支持以下占位符展开：

| 语法 | 说明 |
|---|---|
| `{VAR}` | 取环境变量 VAR，空则返回空 |
| `{VAR:-default}` | 取环境变量 VAR，空则返回 default |
| `${VAR}` / `$VAR` | 标准 os.ExpandEnv |
| `@now` | 运行时时间戳（仅 text 后端），格式 RFC3339 |
| `{SVC_HOME}` 等 | 应用内部环境变量（见 [service] 段） |

### 基础配置

| 键 | 默认 | 说明 |
|---|---|---|
| `port` | `7080` | 监听端口（SSL 时 HTTPS = port+1） |
| `use_ssl` | `false` | 启用自签名 HTTPS |
| `login_authz` | `false` | 启用 401/403→登录页重定向 + 退出按钮注入（响应阶段） |
| `login_token` | 空 | Cookie 校验值；非空时启用请求阶段拦截 |
| `login_timeout` | `0` | Cookie 有效期（秒）；`0`=session 明文模式，`>0`=哈希+过期+自动续期，`<0` 启动报错 |
| `cookie` | `kvs` | Cookie 名 |

### `[proxies]` 段（必填）

每行一个路由 `prefix = url`，**按文件顺序匹配**。

前缀标记：
- 普通 `/path/` → 前缀匹配
- `&/path/` → kvs 托管的服务后端（触发自动部署、loading 页）
- `^pattern` → 正则表达式匹配
- `http://` 或 `https://` 开头 → 全域名匹配

> **服务后端必须以 `&` 开头显式标记**，无隐式提升。

#### 后端协议

`url` 的 scheme 决定后端类型，支持 4 种：

| 协议 | 格式 | 说明 |
|---|---|---|
| `http` / `https` | `http://host:port` | 反向代理到 HTTP 后端 |
| `unix` | `unix:///path/to/sock` | 反向代理到 Unix domain socket |
| `file` | `file:///var/www` | 静态文件服务器（`http.FileServer`） |
| `text` | `text://任意文本` | 直接返回文本内容；支持 `@now` 替换为当前时间（RFC3339） |

```ini
[proxies]
/__healthz=text://OK:@now
/api/=http://api:8080
/cdn/=file:///var/www
&/=unix:///var/run/app.sock
```

### 内联路由 `-n`

`-n` 参数用 `;` 分割多条 `prefix=url`，替代配置文件的 `[proxies]` 段。出现 `-n` 时自动补充 `-c default`，且跳过 `[service]` 段的服务生命周期（版本解析、下载、command 启动）。

```bash
# 单后端
kvs -n "/=http://127.0.0.1:8080"

# 多后端，; 分割
kvs -n "/healthz=text://OK:@now;/=unix:///var/run/vscode.sock"

# 带 service 后端 (& 前缀)
kvs -n "&/=unix:///var/run/app.sock"
```

### `[service]` 段

服务自动部署配置。值支持 `{VAR}` 展开，其中 `SVC_*` 为内部环境变量。

| 键 | 内部变量 | 说明 |
|---|---|---|
| `check` | | 防重复启动检测（http/unix/file 协议） |
| `home` | `SVC_HOME` | 工作目录基准，最先加载 |
| `version_base_url` | `SVC_VERSION_BASE_URL` | 版本 API 基础 URL |
| `version` | `SVC_VERSION` | 版本号；留空则用 `version_latest_url` 获取。可通过 `KVS_VSCODE_VERSION` 环境变量指定 |
| `version_latest_url` | | 获取最新版本的 URL；`#field` 后缀提取 JSON 字段 |
| `version_hash_url` | `SVC_VERSION_HASH` | 获取版本哈希的 URL；支持 `{SVC_VERSION}` 占位；`#field` 提取 JSON 字段；留空则 hash=version |
| `download` | | 下载地址（优先级最高） |
| `download_info` | | 版本信息 API（返回 JSON） |
| `download_field_url` | | download_info JSON 中 URL 字段名，默认 `url` |
| `download_proxy` | | 下载代理（留空=直连）；支持 `http://`、`https://`、`socks5://` |
| `cache_dir` | | 缓存目录 |
| `proxy_path` | | 外部资源代理缓存路径前缀；默认空（禁用），设为 `/__cache/` 启用 |
| `bin_home` | `SVC_BIN_HOME` | 解压后 bin 目录 |
| `init_shell` | | 启动前脚本（每次部署执行一次） |
| `stop_shell` | | 退出前脚本（每次终止执行一次，仅 kvs 管理的后端） |
| `command` | | 后端子进程启动命令 |

#### 自动部署流程

1. `check` 检测后端是否已存在（http/unix/file）→ 存在则视为外部系统服务，kvs 不启动/停止该进程，仅代理转发
2. `bin_home` 目录存在且非空 → 跳过下载解压
3. 解析下载地址：`download` 优先，否则 `download_info` + `download_field_url`
4. 跟随重定向获取 `.ext` → `SVC_PACKAGE_EXT`
5. 下载到 `{cache_dir}/cache/version/{version}_{version_hash}.{ext}`
6. 解压到 `bin_home`
7. 执行 `init_shell`

#### 内部环境变量

在 `[service]` 段中可引用的内部变量（按加载顺序）：

| 变量 | 来源 |
|---|---|
| `SVC_HOME` | `home` |
| `SVC_VERSION_BASE_URL` | `version_base_url` |
| `SVC_VERSION` | `version`（URL 时 fetch 后取字段） |
| `SVC_VERSION_HASH` | `version_hash_url`（留空则 = `SVC_VERSION`） |
| `SVC_PACKAGE_EXT` | download 重定向 URL 的扩展名 |
| `SVC_BIN_HOME` | `bin_home` |

### `[headers]` 段

请求头改写：`Xxx=Val` 设置/覆盖，`Xxx=` 删除。

```ini
[headers]
x-forwarded-port = 443
X-Real-IP = ${REMOTE_ADDR}
```

### `[mirror]` 段

S3 镜像同步配置，供 `kvs mirror` 命令使用。

| 键 | 说明 |
|---|---|
| `vsc_platform` | VS Code 平台标识（默认 `server-linux-x64-web`） |
| `vsc_base_url` | API 基础 URL（默认 `https://update.code.visualstudio.com`） |
| `vsc_download` | 下载 URL 模板，支持 `{name}` 和 `{hash}` 占位；留空则用 API 返回的 url |
| `s3_prefix` | S3 存储前缀（如 `https://oss.example.com/vsc`）；留空则禁用 mirror |
| `s3_access` | S3 access key ID |
| `s3_secret` | S3 secret access key |
| `s3_region` | S3 region（默认空） |

---

## Cookie 认证

### 1. 请求阶段拦截（`login_token` 非空时生效）

设置 `login_token` 后，所有非 `/__` 前缀路径校验 Cookie。`/__login`、`/__logout`、`/__cache/` 等始终放行。常数时间比较防时序攻击。

`login_timeout` 控制两种模式：

| 值 | Cookie 格式 | 校验方式 | 过期 |
|---|---|---|---|
| `0` / 空 | `login_token` 明文 | 直接比对 | session 生命周期 |
| `>0` | `<hash>.<ts>.<salt>` | `sha256(ts+salt+login_token)[:24]` 比对 + 过期检查 | 剩余 ≤1/4 时间自动续期 |

哈希模式下，`salt` 为 16 位随机 hex，`ts` 为签发时间戳（秒）。续期时重新签发 cookie，实现滑动过期。

`<0` 启动报错。

### 2. 响应阶段重定向（需 `login_authz = true`）

后端返回 401/403 → 替换为登录页。`/__logout` 清除 Cookie。

> **注意**：`login_token` 为空时请求阶段无拦截，`login_authz` 仅在后端自身返回 401/403 时生效。

---

## 外部资源代理 (`/__cache/`)

需同时设置 `cache_dir`（磁盘目录）和 `proxy_path`（路由前缀，默认空=禁用）。

```
/__cache/[cc~]{scheme}:{host}[/path][?query]
```

- `cc~` 前缀 = 缓存（仅 GET 2xx）
- 缓存布局：`{cache_dir}/cache/ccproxy/{scheme}:{host}/path`

---

## HTTPS / TLS

`use_ssl = true`：ECDSA P-256 自签名证书，HTTPS 端口 = `port + 1`。

---

## 已知限制

- `command` 不支持带空格的参数
- 外部代理缓存无大小限制
