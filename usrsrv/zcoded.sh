#!/bin/bash

## 这是 zc 官方版本，并且在使用中自动加载最新版本
## vscode - kvs 反向代理 (端口 KVS_ZCODED_PORT:7088, 目录 ${KVS_ZCODED_HOME:-~/.vscode-server}, PASSWORD:无需密码)

# source /etc/profile

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

if [[ -z "${KVS_ZCODED_HOME}" ]]; then
    # export KVS_ZCODE_HOME=`echo ~/.vscode-server`
    export KVS_ZCODED_HOME="/wsc/.vsc"
    echo "KVS_ZCODED_HOME is empty, default is ${KVS_ZCODED_HOME}"
fi

# kvs 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start zcoded server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_HOME="${KVS_ZCODED_HOME}" KVS_LOGIN_AUTHZ=true \
KVS_PORT="${KVS_ZCODED_PORT:-7088}" KVS_COOKIE=zcoded-tkn KVS_LOGIN_TOKEN="${PASSWORD}" \
exec kvs -c zcoded
