#!/bin/bash

## =======================================================================

echo "======== init git config."
# git config pull.rebase false
if [ $GIT_USER_NAME ]; then
    git config --global user.name "$GIT_USER_NAME"
fi
if [ $GIT_USER_EMAIL ]; then
    git config --global user.email "$GIT_USER_EMAIL"
fi

## =======================================================================

if [ ! -f '/etc/ssh/_init' ]; then
    echo `date -Iseconds` > /etc/ssh/_init
    echo '======== init sshd keys.'
    mkdir /run/sshd
    
    # ssh-keygen 初始化
    echo y | ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_rsa_key
    echo y | ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_dsa_key
    echo y | ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_ecdsa_key
    echo y | ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_ed25519_key
    echo ''
    
    # 配置 ssh 的登录密码
    if [[ -n "${USER}" ]] && [[ "$USER" != "root" ]] && [[ -n "${PASSWORD}" ]]; then
        echo "set ssh password for ${USER} ..."
        echo "${USER}:${PASSWORD}" | chpasswd
    fi
    echo "PasswordAuthentication yes" >> /etc/ssh/sshd_config
    
    # 配置 root 用户的 ssh password
    if [[ -n "${PASSROOT}" ]]; then
        echo 'set ssh password for root ...'
        echo "root:${PASSROOT}" | chpasswd
        echo "PermitRootLogin yes" >> /etc/ssh/sshd_config
    fi
fi

## =======================================================================

echo "======== end for init script."