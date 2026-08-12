# kssh

基于登录名进行 SSH 分流、中继与代理。

客户端通过用户名携带目标信息，kssh 据此解析出目标地址，建立到目标的 SSH 连接，
并在客户端与目标之间双向转发 channel 与 global request。

## 登录格式

```
server-name[/user-name][:password]@proxy-host
```

- `server-name` 既可以是单纯主机名，也可以是 `host-port-ssvc{snum}` 复合形式
- `user-name` 默认值为 `user`
- `password` 可选，两种指定方式（二选一）：
  - **内嵌**：用户名中冒号分隔 `server-name/user:password`，连接时无需再输入
  - **后输入**：用户名中不含密码 `server-name/user`，认证阶段客户端输入密码

`server-name` 三种形态：

| 形态 | 示例 | 解析出的占位符 |
| --- | --- | --- |
| `host` | `1.2.3.4` | `{host}` |
| `host-port` | `1.2.3.4-2222` | `{host}` `{port}` |
| `host-port-ssvc{snum}` | `1.2.3.4-2222-vsc0` | `{host}` `{port}` `{ssvc}` `{snum}` |

## 依赖

- `golang.org/x/crypto/ssh`（Go 官方扩展库，SSH 协议实现）
- 日志使用标准库 `log/slog`

不引入任何第三方组件。

## 环境变量

| 变量 | 说明 | 默认值 |
| --- | --- | --- |
| `KEYS_FOLDER` | 主机密钥目录 | `/etc/ssh` |
| `LISTEN_ADDR` | 监听地址 | `:22` |
| `TARGET_ADDR` | 目标地址模板 | `{host}:22` |

`TARGET_ADDR` 支持占位符：`{host}` `{port}` `{ssvc}` `{snum}`。

## 使用

```bash
# 构建
go build

# 启动服务（默认监听 :22，目标模板 {host}:22）
go run main.go

# 自定义配置
KEYS_FOLDER=/etc/ssh LISTEN_ADDR=:2222 TARGET_ADDR='{host}:22' go run main.go
```

## 测试

```bash
# 方式一：内嵌密码（用户名中冒号分隔）
#   server-name = x.x.x.x （作为 {host}）
#   user        = root
#   password    = pass
ssh -o StrictHostKeyChecking=no x.x.x.x/root:pass@127.0.0.1

# 方式二：后输入密码（用户名中不含密码，认证阶段输入）
ssh -o StrictHostKeyChecking=no x.x.x.x/root@127.0.0.1
# 客户端会提示输入密码，输入的密码将作为目标 SSH 登录密码
```

集群内部复合地址示例（用于 suisrc/webtop | suisrc/vscode）：

```bash
# 模板：{host}-0.vsc-{ssvc}-dev.ws{snum}.svc.cluster.local:{port}
# 连接：host-port-ssvc{snum}/user:pass@127.0.0.1
# 例如 1.2.3.4-2222-vsc0/root:pass@127.0.0.1
# 解析后目标：1.2.3.4-0.vsc-vsc-dev.ws0.svc.cluster.local:2222
```

## 生成主机密钥

```bash
# 在指定目录生成 RSA/ECDSA/Ed25519 主机密钥
ssh-keygen -A -f /etc/ssh
```

## 代码结构

单文件 `main.go`，`package main`，基于 [suisrc/sshproxy](https://github.com/suisrc/sshproxy) 算法，仅认证部分扩展为双认证模式：

| 部分 | 说明 |
| --- | --- |
| `Proxy` | 服务生命周期：`Start`/`Stop`/`Wait`/`serve`/`InitConf`/`AddHostKey` |
| `InitConf` | 双认证配置：`NoClientAuth` + `NoClientAuthCallback` + `PartialSuccessError` + `PasswordCallback` |
| `HandleSshConn` | 连接处理：握手 → 解析用户名/密码 → 解析地址 → 拨号 → 转发 |
| `ForwardRequest` | 全局请求双向转发 |
| `ForwardChannel` | channel 数据流与请求双向转发 |
| `passKey` | `Permissions.Extensions` 中存放客户端输入密码的 key |

## 认证流程

双认证模式，兼容内嵌密码与后输入密码：

```
客户端连接 → none 认证
  ├─ 用户名含 ':'（内嵌密码）→ 直接通过
  └─ 用户名不含 ':'          → PartialSuccessError → 密码认证
                                                          └─ 客户端输入密码 → 通过
                                                          └─ 密码经 Permissions.Extensions 传递给 HandleSshConn
```

密码来源（`HandleSshConn` 内）：

- **内嵌**：用户名含 `:`，冒号后即为密码，同时截掉冒号及密码部分得到纯净用户名
- **后输入**：用户名不含 `:`，从 `cConn.Permissions.Extensions[passKey]` 读取

## Channel 转发关闭编排

`ForwardChannel` 中每条 channel 共 6 条 goroutine（stdout×2 + stderr×2 + request×2），关闭顺序至关重要：

```
stdout EOF → CloseWrite（只关写端，exit-status 等请求仍可从读端通过）
           → wg.Done（通知本侧 stderr + request 可收尾）
           → wg.Wait（等本侧 stderr + request 全部完成）
           → Close（最终关闭，此时 exit-status 已送达）
```

每侧（origin/target）各有一个 `WaitGroup(3)` 编排本侧的 stdout + stderr + request，
确保 `Close()` 在 `exit-status` 等 channel 级请求转发完成之后才执行。
这是局域网低延迟环境下 VS Code Remote-SSH 能正确解析 "remote port" 的关键。

## 与参考 sshproxy 的差异

仅以下两处，其余转发算法完全一致：

1. **`InitConf`**：参考仅 `NoClientAuth: true`（只支持内嵌密码），扩展为双认证
2. **`HandleSshConn`**：参考用 `SplitN(":", 2)` 强制要求内嵌密码，扩展为双来源（内嵌优先，否则读 `Permissions.Extensions`）

日志从 `logrus` 改为标准库 `slog`，去掉第三方依赖。
