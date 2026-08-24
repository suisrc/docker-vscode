#!/usr/bin/env bash
# ============================================================================
# install.sh - 安装 kvs 反向代理并注册 vscode 服务
# ============================================================================
#
# 功能:
#   1. 从 github.com 下载最新版本的 kvs 到 /root/.vsc/kvs
#   2. 生成 vscode.service (要求输入 password, 为空则随机 12 位密码)
#   3. systemctl daemon-reload / enable / start
#
# 使用:
#   ./install.sh                      # 交互输入密码
#   PASSWORD=mypass ./install.sh      # 直接指定密码
#   KVS_REPO=owner/repo ./install.sh  # 指定 kvs 所在的 github 仓库
#
# ============================================================================
# 默认工作目录 /root/.vsc/
# scp tools/kvs/kvs root@x.x.x.x:/root/.vsc/
#
# 安装方法:
#
#   1. 将本文件复制到 systemd 目录:
#        cp /root/.vsc/vscode.service /etc/systemd/system/vscode.service
#
#   2. 重新加载 systemd 配置:
#        systemctl daemon-reload
#
#   3. 设置开机自启并立即启动:
#        systemctl enable vscode.service
#        systemctl start  vscode.service
#
#   常用管理命令:
#        systemctl status  vscode.service   # 查看状态
#        systemctl stop    vscode.service   # 停止
#        systemctl restart vscode.service   # 重启
#        journalctl -u vscode.service -f    # 查看日志
# ============================================================================

set -euo pipefail

# ----------------------------------------------------------------------------
# 可配置项
# ----------------------------------------------------------------------------

# kvs 所在的 GitHub 仓库 (owner/repo)，可通过环境变量覆盖
KVS_REPO="${KVS_REPO:-suisrc/docker-vscode}"

# 安装目录
KVS_INSTALL_DIR="${KVS_INSTALL_DIR:-/root/.vsc}"
KVS_BIN="${KVS_INSTALL_DIR}/kvs"

# 监听端口 (SSL 时 HTTPS = port+1)
KVS_PORT="${KVS_PORT:-7080}"

# 登录令牌 (来自 PASSWORD，默认随机 12 位)
KVS_LOGIN_TOKEN="${PASSWORD:-}"

# socket 文件路径
KVS_SVC_SOCK_FILE="${KVS_SVC_SOCK_FILE:-/var/run/vscode.sock}"

# wss 场景转发端口
KVS_SVC_HEADER_X_FORWARDED_PORT="${KVS_SVC_HEADER_X_FORWARDED_PORT:-7081}"

# 服务名
SERVICE_NAME="vscode.service"

# ----------------------------------------------------------------------------
# 辅助函数
# ----------------------------------------------------------------------------

log()  { printf '[install] %s\n' "$*"; }
err()  { printf '[install] ERROR: %s\n' "$*" >&2; }

# 生成随机密码
gen_password() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 16 | tr -dc 'A-Za-z0-9' | head -c 12
  else
    tr -dc 'A-Za-z0-9' </dev/urandom | head -c 12
  fi
}

# 需要 root 权限
require_root() {
  if [[ "$(id -u)" != "0" ]]; then
    err "root privileges required, please run with sudo."
    exit 1
  fi
}

# ----------------------------------------------------------------------------
# 1. 获取最新版本并下载 kvs
# ----------------------------------------------------------------------------
download_kvs() {
  log "Fetching latest kvs version: ${KVS_REPO}"
  local release_url="https://api.github.com/repos/${KVS_REPO}/releases/latest"

  # 解析平台 (默认 linux amd64)
  local arch
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) err "unsupported architecture: $(uname -m)"; exit 1 ;;
  esac

  # 通过 API 查询最新 release 的 JSON
  local api_json
  if command -v curl >/dev/null 2>&1; then
    api_json="$(curl -fsSL "$release_url")"
  elif command -v wget >/dev/null 2>&1; then
    api_json="$(wget -qO- "$release_url")"
  else
    err "curl or wget is required"
    exit 1
  fi

  # 从 assets 里匹配包含 kvs + 当前架构 的二进制文件
  local asset_url
  asset_url="$(echo "$api_json" \
    | grep -oE '"browser_download_url":\s*"[^"]*"' \
    | sed -E 's/.*"browser_download_url":\s*"([^"]*)"/\1/' \
    | grep "$arch" | grep -i 'kvs' | head -n1)"

  if [[ -z "$asset_url" ]]; then
    err "no kvs (${arch}) binary found in ${KVS_REPO} latest release."
    err "check KVS_REPO, or specify the repository via KVS_REPO."
    exit 1
  fi

  log "Downloading kvs: ${asset_url}"
  mkdir -p "$KVS_INSTALL_DIR"

  # 下载二进制
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$asset_url" -o "${KVS_BIN}.tmp"
  else
    wget -qO "${KVS_BIN}.tmp" "$asset_url"
  fi

  chmod +x "${KVS_BIN}.tmp"
  mv "${KVS_BIN}.tmp" "$KVS_BIN"
  log "kvs installed to ${KVS_BIN}"
}

# ----------------------------------------------------------------------------
# 2. 生成 vscode.service (在脚本内直接生成)
# ----------------------------------------------------------------------------
generate_service() {
  # 若无密码则随机生成 12 位
  if [[ -z "$KVS_LOGIN_TOKEN" ]]; then
    KVS_LOGIN_TOKEN="$(gen_password)"
    log "no password provided, generated random one: ${KVS_LOGIN_TOKEN}"
  fi

  local service_file="/etc/systemd/system/${SERVICE_NAME}"

  log "Generating service file: ${service_file}"
  cat >"$service_file" <<EOF
# ============================================================================
# ${SERVICE_NAME} - systemd 服务单元，通过 systemctl 管理 vscode server
# ============================================================================
#
# 自动生成的安装文件，由 install.sh 创建
#
# 常用管理命令:
#   systemctl status  ${SERVICE_NAME}   # 查看状态
#   systemctl stop    ${SERVICE_NAME}   # 停止
#   systemctl restart ${SERVICE_NAME}   # 重启
#   journalctl -u ${SERVICE_NAME} -f    # 查看日志
#
# 登录密码: ${KVS_LOGIN_TOKEN}
# ============================================================================

[Unit]
Description=VS Code Server (kvs reverse proxy)
After=network.target
Wants=network.target

[Service]
Type=simple
ExecStart=${KVS_BIN} -c default

# ---- 缓存变量 (kvs 启动所需的环境变量) ----
# socket 文件路径
Environment=KVS_SVC_SOCK_FILE=${KVS_SVC_SOCK_FILE}
# vscode server 数据目录 (缓存/版本/serve 均在此下)
Environment=KVS_HOME=${KVS_INSTALL_DIR}
# 启用 401/403 -> 登录页重定向 + 退出按钮注入
Environment=KVS_LOGIN_AUTHZ=true
# 监听端口
Environment=KVS_PORT=${KVS_PORT}
Environment=KVS_USESSL=true
# 静态 Cookie 校验值
Environment=KVS_COOKIE=vscode-tkn
# 登录令牌 (来自 PASSWORD)
Environment=KVS_LOGIN_TOKEN=${KVS_LOGIN_TOKEN}
# wss 场景需设置转发端口
Environment=KVS_SVC_HEADER_X_FORWARDED_PORT=${KVS_SVC_HEADER_X_FORWARDED_PORT}

Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

  log "password: ${KVS_LOGIN_TOKEN}"
  log "keep this password safe, it is used to log in to vscode."
}

# ----------------------------------------------------------------------------
# 3. 注册并启动服务
# ----------------------------------------------------------------------------
enable_service() {
  log "Reloading systemd config"
  systemctl daemon-reload

  log "Enabling ${SERVICE_NAME} at boot"
  systemctl enable "${SERVICE_NAME}"

  log "Starting ${SERVICE_NAME}"
  systemctl start "${SERVICE_NAME}"

  log "Service status:"
  systemctl --no-pager --full status "${SERVICE_NAME}" || true
}

# ----------------------------------------------------------------------------
# 主流程
# ----------------------------------------------------------------------------
main() {
  require_root
  download_kvs
  generate_service
  enable_service
  log "Installation complete. See kvs docs for access (default port ${KVS_PORT})."
}

main "$@"
