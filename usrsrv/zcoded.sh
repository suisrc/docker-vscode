#!/bin/bash

## 软件位置 ls /wsc/.vsc/zcode
# source /etc/profile

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

if [[ -z "${KVS_ZCODEX_HOME}" ]]; then
    # export KVS_ZCODE_HOME=`echo ~/.vscode-server`
    export KVS_ZCODEX_HOME="/wsc/.vsc/zcode"
    echo "KVS_ZCODEX_HOME is empty, default is ${KVS_ZCODEX_HOME}"
fi

if [[ -z "${KVS_ZCODEX_VERSION}" ]]; then
    version_file="$KVS_ZCODEX_HOME/.version"
    if [[ ! -f "$version_file" ]]; then
      echo "错误: 未找到 $version_file, 请先部署发行包，确定部署文件夹跟目录下存在 .version 文件" >&2
      exit 1
    fi
    export KVS_ZCODEX_VERSION=$(<"$version_file")
    echo "KVS_ZCODEX_VERSION is empty, .version is ${KVS_ZCODEX_VERSION}"
fi

# 出现系统中可用的 node 的全路径
if [[ -z "$KVS_ZCODEX_NODE" ]]; then
    if command -v node >/dev/null 2>&1; then
        export KVS_ZCODEX_NODE=$(command -v node)
    elif [[ -d "/root/.nvm/versions/node" ]]; then
        # nvm 安装：取版本号最高的那个
        export KVS_ZCODEX_NODE=$(ls -d /root/.nvm/versions/node/*/bin/node 2>/dev/null | sort -V | tail -1)
    fi
fi
if [[ -z "$KVS_ZCODEX_NODE" ]]; then
    echo "错误: 未找到可用的 node，请安装 node" >&2
    exit 1
fi
echo "node: $KVS_ZCODEX_NODE"

# node /wsc/.vsc/zcode/3.14.0/zcode/bin/zcode.mjs --web --help
# zcode --web [--host <host>] [--port <port>] [--workspace <path>] [--open|--no-open] [--token <token>|--no-token]
# kvs 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start zcodex server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_HOME="${KVS_ZCODEX_HOME}" KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN="${PASSWORD}"  \
KVS_PORT="${KVS_ZCODEX_PORT:-7088}" KVS_COOKIE_TOKEN_KEY=zcodex-tkn KVS_QUERY_TOKEN_KEY=tkn \
KVS_SVC_BIN_HOME="${KVS_ZCODEX_HOME}/${KVS_ZCODEX_VERSION}/zcode" KVS_ZCODE_PORT="7587" \
exec kvs -c zcodex
