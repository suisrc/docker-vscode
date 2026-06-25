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

# start vscode server
if [[ -z "${PASSWORD}" ]]; then
    echo 'start vscode server with http without password.'
    exec codez serve-web --accept-server-license-terms \
        --host ${VSC_HOST:-127.0.0.1} --port ${VSC_PORT:-7080} \
        --cli-data-dir ${VSC_HOME:-/vsc} \
        --server-data-dir ${VSC_HOME:-/vsc} \
        --default-folder ${DEFAULT_FOLDER:-/app} \
        --without-connection-token \
        $VSC_ARGS
    # exec 结束后会直接替换当前进程，所以后续的代码不会执行
fi

export VSC_CORS_IDX=cache+https://www.vscode-unpkg.net
export VSC_CORS_SUF_browser_workbench_workbench_js=https://main.vscode-cdn.net,https://www.vscode-unpkg.net
# codea 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start vscode server with unix socket with password.'
exec codea --backend unix://${VSC_SOCK:-/var/run/vscode.sock} --service "\
codez serve-web --accept-server-license-terms \
    --socket-path ${VSC_SOCK:-/var/run/vscode.sock} \
    --cli-data-dir ${VSC_HOME:-/vsc} \
    --server-data-dir ${VSC_HOME:-/vsc} \
    --default-folder ${DEFAULT_FOLDER:-/app} \
    --connection-token ${PASSWORD} \
    $VSC_ARGS"

# VSCODE_CLI_UPDATE_URL=http://127.0.0.1:7080/__vscode
# PROXY_HEADER_x-forwarded-port=443