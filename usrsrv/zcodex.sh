#!/bin/bash

## source /etc/profile

## 集成 vscode, zcode
## KVS_ZCODEX_PORT:7088  KVS_ZCODEX_HOME:/wsc/.vsc

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

# 提供 zcode web application 下载地址
# KVS_SVC_ZCODE_DEF="sims" # commit前 必须注释，必须注释，必须注释
if [[ "${KVS_SVC_ZCODE_DEF:-}" == "sims" ]]; then
    export KVS_SVC_ZCODE_VERSION_LATEST_URL=https://oss.vsc.sims-cn.com/vsc/zcode/latest.json#version
    export KVS_SVC_ZCODE_DOWNLOAD=https://oss.vsc.sims-cn.com/vsc/zcode/releases/VERSION/zcode-VERSION.tar.gz
fi

# 提供 vscode 本地默认打开插件
if [[ -z "$KVS_SVC_VSCODE_OPEN_URL" ]]; then
    export KVS_SVC_VSCODE_OPEN_URL=vscode://ms-vscode.remote-server/open
fi

# 查询系统中可用的 node 的全路径
if [[ -z "$KVS_SVC_NODE" ]]; then
    if command -v node >/dev/null 2>&1; then
        export KVS_SVC_NODE=$(command -v node)
    elif [[ -d "/root/.nvm/versions/node" ]]; then
        # nvm 安装：取版本号最高的那个
        export KVS_SVC_NODE=$(ls -d /root/.nvm/versions/node/*/bin/node 2>/dev/null | sort -V | tail -1)
    fi
fi
if [[ -z "${KVS_SVC_NODE:-}" ]]; then
    echo "错误: 未找到可用的 node，请安装 node" >&2
    exit 1
fi
echo "node: ${KVS_SVC_NODE}"

if [[ -z "${KVS_ZCODEX_HOME}" ]]; then
    # export KVS_ZCODEX_HOME=`echo ~/.vscode-server`
    export KVS_ZCODEX_HOME="/wsc/.vsc"
    echo "KVS_ZCODEX_HOME is empty, default is ${KVS_ZCODEX_HOME}"
fi

echo 'start zcodex server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_HOME="${KVS_ZCODEX_HOME}" KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN="${PASSWORD}" \
KVS_PORT="${KVS_ZCODEX_PORT:-7088}" KVS_COOKIE_TOKEN_KEY=vscode-tkn KVS_QUERY_TOKEN_KEY=tkn \
ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=ws://127.0.0.1:7088/remote/ws \
exec kvs -n :zcodex
