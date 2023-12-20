#!/usr/bin/with-contenv with-user

## 这是 ms 官方版本，并且在使用中自动加载最新版本

## check vscode server
if [[ "${SVC_VSCODE}" == "0" ]]; then
    echo 'disable vscode server ...'
    sleep 1
    sudo s6-rc stop svc-vscode
    exit
fi

## init vscode config
if [ ! -f "$HOME/.vscode_server_init" ]; then
    echo `date` > $HOME/.vscode_server_init
    echo 'init vscode config ...'
    if [ $GIT_USER_NAME ]; then
        git config --global user.name "$GIT_USER_NAME" #git config pull.rebase false
        if [ $GIT_USER_EMAIL ]; then
            git config --global user.email "$GIT_USER_EMAIL"
        fi
    fi
fi

echo 'start vscode server ...'
## VSC_ARGS: vscode参数
if [[ "${SVC_VSCODE}" == "-1" ]]; then
    # tun server， 注册到vscode官方服务器, PS：未经测试，延迟高，而且一个账号最多5个设备
    exec code-cli tunnel --accept-server-license-terms --random-name --cli-data-dir ${VSC_HOME}
else 
    # web server， 本地运行web服务
    exec code-cli serve-web --accept-server-license-terms --host ${VSC_HOST:-0.0.0.0} --port ${VSC_PORT:-6801} --cli-data-dir ${VSC_HOME} --server-data-dir ${HOME}/.vscode-server $VSC_ARGS
fi


