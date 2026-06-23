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
if [ ! -f "$HOME/.vscode_init" ]; then
    echo `date` > $HOME/.vscode_init
    echo 'init vscode config ...'
    # git config pull.rebase false
    if [ $GIT_USER_NAME ]; then
        git config --global user.name "$GIT_USER_NAME"
    fi
    if [ $GIT_USER_EMAIL ]; then
        git config --global user.email "$GIT_USER_EMAIL"
    fi
fi

echo 'start vscode server ...'
exec code-cli serve-web --accept-server-license-terms \
    --socket-path /home/webtop/.vscode.sock \
    --cli-data-dir ${VSC_HOME}/vscdir \
    --server-data-dir ${VSC_HOME}/vscode \
    --default-folder ${DEFAULT_FOLDER} \
    $VSC_ARGS
