#!/bin/bash

## 这是 ms 官方版本，并且在使用中自动加载最新版本

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

# 判断 "${VSCODE_WSC:-/wsc}/.vsc/data/Machine/settings.json" 是否存在，如果不存在则创建一个默认的 settings.json 文件
if [ ! -f "${VSCODE_WSC:-/wsc}/.vsc/data/Machine/settings.json" ]; then
    mkdir -p "${VSCODE_WSC:-/wsc}/.vsc/data/Machine"
    cat <<EOF > "${VSCODE_WSC:-/wsc}/.vsc/data/Machine/settings.json"
{
    "chat.allowAnonymousAccess": true,
    "terminal.integrated.scrollback": 10000,
    "terminal.integrated.defaultProfile.linux": "zsh",
    "git.ignoreLegacyWarning": true,
    "git.enableSmartCommit": true,
    "files.autoSave": "off",
    "editor.renderWhitespace": "all",
    "editor.suggestSelection": "first",
    "editor.fontSize": 16,
    "editor.fontLigatures": false,
    "explorer.confirmDelete": false,
    "extensions.autoUpdate": "off",
    "extensions.autoCheckUpdates": false,
    "workbench.colorTheme": "Dark+"
}

EOF
fi

# 判断 VSCODE_INIT 是否为空， 如果为空， 则设置默认值
if [[ -z "${VSCODE_INIT}" ]]; then
    export VSCODE_INIT='sed -i \
    -e "s|https://www.vscode-unpkg.net/nls/||g" \
    -e "s|https://www.vscode-unpkg.net/_lp/||g" \
    -e "s|https://main.vscode-cdn.net/extensions/marketplace.json||g" \
    -e "s|https://main.vscode-cdn.net/mcp/servers.json||g" \
    -e "s|https://main.vscode-cdn.net/extensions/chat.json||g" \
    ${SERVICE_DIR}/product.json'
fi

# VSCODE_HASH 不存在，则设置默认值为 vscode:latest
if [[ -z "${VSCODE_HASH}" ]]; then
    export VSCODE_HASH="vscode:latest"
fi

# kin 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start vscode server. wss need set env: PROXY_HEADER_x-forwarded-port=443'
SERVICE_SET=${VSCODE_INIT} SERVICE_WSC=${VSCODE_WSC:-/wsc} \
SERVICE_CMD='${SERVICE_DIR}/bin/code-server --accept-server-license-terms --without-connection-token \
  --server-data-dir ${SERVICE_WSC}/.vsc --socket-path /var/run/vscode.sock' \
exec kin --use-ssl --backend "/__healthz=text://OK:{now};^/=unix:///var/run/vscode.sock" --authz --token "${PASSWORD}"
