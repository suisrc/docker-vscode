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
if [[ -z "${PASSWORD}" ]]; then
    echo 'start vscode server with http without password ...'
    exec codez serve-web --accept-server-license-terms \
        --host ${VSC_HOST:-127.0.0.1} --port ${VSC_PORT:-7080} \
        --cli-data-dir ${VSC_HOME:-/vsc} \
        --server-data-dir ${VSC_HOME:-/vsc} \
        --default-folder ${DEFAULT_FOLDER:-/app} \
        --without-connection-token \
        $VSC_ARGS
    # exec 结束后会直接替换当前进程，所以后续的代码不会执行
fi

echo 'start vscode server with unix socket with password ...'
rm -f ${VSC_SOCK:-/var/run/vscode.sock} # 删除 sock
codez serve-web --accept-server-license-terms \
    --socket-path ${VSC_SOCK:-/var/run/vscode.sock} \
    --cli-data-dir ${VSC_HOME:-/vsc} \
    --server-data-dir ${VSC_HOME:-/vsc} \
    --default-folder ${DEFAULT_FOLDER:-/app} \
    --connection-token ${PASSWORD}
    $VSC_ARGS &
PID1=$!
BACKEND_URL="unix://${VSC_SOCK:-/var/run/vscode.sock}" PROXY_PORT=${VSC_PORT:-7080} authz &
PID2=$!
# wait for any of the processes to exit
wait -n $PID1 $PID2
EXIT_CODE=$?
echo "vscode server exited with code $EXIT_CODE"
# kill the other process if it's still running
kill $PID1 $PID2 2>/dev/null
# wait for both processes to exit
wait $PID1 $PID2 2>/dev/null
exit $EXIT_CODE