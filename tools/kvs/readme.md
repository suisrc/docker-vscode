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
| `http`/`https`、`ws`/`wss` | 反向代理到 HTTP 后端（升级请求透传） |
| `unix` | Unix domain socket |
| `file` | 静态文件服务器（br/gz/zst 预压缩优选） |
| `text` | 直接返回文本；支持 `@now` 替换当前时间（RFC3339） |
| `wsws://` / `api://` | 进程内 zcode 中继 / handler 注册表（zcodex 预设使用，见 [agents.md](agents.md)） |

## 外部资源代理 (`/__cache/`)

需同时设置 `cache_dir`（磁盘目录）和 `proxy_path`（路由前缀，默认空=禁用，设为 `/__cache/` 启用）。

```
/__cache/[cc~]{scheme}:{host}[/path][?query]
```

`cc~` 前缀 = 缓存（仅 GET 2xx），布局 `{cache_dir}/cache/ccproxy/{scheme}:{host}/path`。`cache_sed` 缓存内容替换（规则格式、once/each 动态 Host 模式）见 demo 模板注释与 [agents.md](agents.md)。

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
