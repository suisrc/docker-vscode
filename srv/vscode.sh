#!/bin/bash

## 这是 ms 官方版本，并且在使用中自动加载最新版本

# source /etc/profile

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

# kvs 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start vscode server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_SVC_SOCK_FILE=/var/run/vscode.sock KVS_HOME="${VSCODE_WSC:-/wsc}/.vsc" KVS_LOGIN_AUTHZ=true \
KVS_PORT="${VSCODE_PORT:-7080}" KVS_COOKIE=vscode-tkn KVS_LOGIN_TOKEN="${PASSWORD}" \
exec kvs -c default
