#!/usr/bin/with-contenv with-user

## 这是 ms 官方版本，并且在使用中自动加载最新版本

## check vscode server
if [[ "${SVC_VSCODE}" == "0" ]]; then
    echo 'disable vscode server.'
    sleep 1
    sudo s6-rc stop svc-vscode
    exit
fi

## init vscode config
if [ ! -f "$HOME/.vscode_init" ]; then
    echo `date` > $HOME/.vscode_init
    echo 'init vscode config.'
    # git config pull.rebase false
    if [ $GIT_USER_NAME ]; then
        git config --global user.name "$GIT_USER_NAME"
    fi
    if [ $GIT_USER_EMAIL ]; then
        git config --global user.email "$GIT_USER_EMAIL"
    fi
fi

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

# PROXY_HEADER_x-forwarded-port=443
# VSCODE_WSC="/wsc/go/github/ws01/docker-vscode/temp" \
# VSCODE_HASH="vscode:latest" \
# VSCODE_INIT='sed -i "s|https://www.vscode-unpkg.net/nls/|/__proxy/cc~http:127.0.0.1:0/nls/|g" ${SERVICE_DIR}/product.json' \
# DEFAULT_FOLDER="/app" \
# PASSWORD=""

# codea 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start vscode server. wss need set env: PROXY_HEADER_x-forwarded-port=443'
codea --use-ssl  \
    --svc-wsc "${VSCODE_WSC:-/wsc}" \
    --svc-pre "${VSCODE_INIT}" \
    --backend "/__healthz=text://ok,{now};^/=unix://@vscode.sock" \
    --svc-cmd '${SERVICE_DIR}/bin/code-server --accept-server-license-terms --socket-path @vscode.sock --server-data-dir ${SERVICE_WSC}/.vscode --connection-token ${PASSWORD}'
