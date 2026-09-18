#!/bin/bash

## 这是 ms 官方版本，并且在使用中自动加载最新版本
## vscode - kvs 反向代理 (端口 KVS_VSCODE_PORT:7080, 目录 ${KVS_VSCODE_HOME:-~/.vscode-server}, PASSWORD:无需密码)

# source /etc/profile

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

if [[ -z "${KVS_ZCODE_HOME}" ]]; then
    # export KVS_ZCODE_HOME=`echo ~/.zcode`
    export KVS_ZCODE_HOME="/wsc/.zco"
    echo "KVS_ZCODE_HOME is empty, default is ${KVS_ZCODE_HOME}"
fi

# kvs 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start zcode server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_HOME="${KVS_ZCODE_HOME}" KVS_LOGIN_AUTHZ=true KVS_COOKIE=zcode-tkn KVS_LOGIN_TOKEN="${PASSWORD}" \
KVS_PORT="${KVS_ZCODE_PORT:-7088}" KVS_PATH_PUBLIC='/ws|/remote/v4|/api/v1/client/configs' \
KVS_CC_SED='src-*.js|`wss://zcode.z.ai/ws`|`wss://>host</ws`||src-*.js|`/api/v1/client/configs`,ff(e).origin|`/api/v1/client/configs`' \
exec kvs -n "ws~/ws=wsws://zcode;cc~/api/v1/=https://zcode.z.ai/api/v1/;cc~/remote/v4=https://zcode.z.ai/remote/v4;/=api://zlist"
