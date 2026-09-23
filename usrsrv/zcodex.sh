#!/bin/bash


## 软件位置 ls /wsc/.vsc/zcode
# source /etc/profile

# ZCODEX_DEF="sims" # commit前 必须注释，必须注释，必须注释
# 测试 使用 ZCODEX_VERSION ZCODEX_HOME ZCODEX_NODE
if [[ "${ZCODEX_DEF:-}" == "sims" ]]; then
    ZCODEX_VERSION_LATEST_URL=https://oss.vsc.sims-cn.com/vsc/zcode/latest.json#version
    ZCODEX_DOWNLOAD=https://oss.vsc.sims-cn.com/vsc/zcode/releases/SVC_VERSION/zcode-SVC_VERSION.tar.gz
fi

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

if [[ -z "${ZCODEX_HOME}" ]]; then
    # export KVS_ZCODE_HOME=`echo ~/.vscode-server`
    export ZCODEX_HOME="/wsc/.vsc/zcode"
    echo "ZCODEX_HOME is empty, default is ${ZCODEX_HOME}"
fi

# 出现系统中可用的 node 的全路径
if [[ -z "$ZCODEX_NODE" ]]; then
    if command -v node >/dev/null 2>&1; then
        export ZCODEX_NODE=$(command -v node)
    elif [[ -d "/root/.nvm/versions/node" ]]; then
        # nvm 安装：取版本号最高的那个
        export ZCODEX_NODE=$(ls -d /root/.nvm/versions/node/*/bin/node 2>/dev/null | sort -V | tail -1)
    fi
fi
if [[ -z "${ZCODEX_NODE:-}" ]]; then
    echo "错误: 未找到可用的 node，请安装 node" >&2
    exit 1
fi
echo "node: ${ZCODEX_NODE}"

# 处理应用版本信息
if [[ -n "${ZCODEX_DOWNLOAD}" ]]; then
    export KVS_SVC_VERSION_LATEST_URL=${ZCODEX_VERSION_LATEST_URL}
    export KVS_SVC_DOWNLOAD=${ZCODEX_DOWNLOAD}
    echo "ZCODEX_DOWNLOAD is ${ZCODEX_DOWNLOAD}"

elif [[ -n "${ZCODEX_VERSION}" ]]; then
    export KVS_SVC_VERSION=${ZCODEX_VERSION}
    echo "ZCODEX_VERSION is ${ZCODEX_VERSION}"

else
    version_file="${ZCODEX_HOME}/.version"
    if [[ ! -f "$version_file" ]]; then
      echo "错误: 未找到 $version_file, 请先部署发行包，确定部署文件夹跟目录下存在 .version 文件" >&2
      exit 1
    fi
    export KVS_SVC_VERSION="$(<"$version_file")"
    if [[ -z "$KVS_SVC_VERSION" ]]; then
        echo "错误: $version_file 内容为空或不可读" >&2
        exit 1
    fi
    echo "KVS_SVC_VERSION by .version is ${KVS_SVC_VERSION}"
fi

# node /wsc/.vsc/zcode/3.14.0/zcode/bin/zcode.mjs --web --help
# zcode --web [--host <host>] [--port <port>] [--workspace <path>] [--open|--no-open] [--token <token>|--no-token]
# kvs 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
# 系统 使用 SVC_VERSION 而不是 ZCODEX_VERSION 使防止应用在运行中更新，发生版本变动

echo 'start zcodex server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_HOME="${ZCODEX_HOME}" KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN="${PASSWORD}"  \
KVS_PORT="${ZCODEX_PORT:-7088}" KVS_COOKIE_TOKEN_KEY=zcodex-tkn KVS_QUERY_TOKEN_KEY=tkn \
KVS_ZCODE_PORT="${KVS_ZCODE_PORT:-7587}" KVS_SVC_BIN_HOME="{ZCODEX_HOME}/{SVC_VERSION}/zcode" \
exec kvs -c zcodex
