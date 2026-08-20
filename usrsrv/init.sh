#!/bin/bash

set -eu
## =======================================================================

echo "======== init git config."
# git config pull.rebase false
if [[ -n "${GIT_USER_NAME:-}" ]]; then
    git config --global user.name "${GIT_USER_NAME}"
fi
if [[ -n "${GIT_USER_EMAIL:-}" ]]; then
    git config --global user.email "${GIT_USER_EMAIL}"
fi

## =======================================================================

if [ ! -f '/etc/ssh/_init' ]; then
    echo "$(date -Iseconds)" > /etc/ssh/_init
    echo '======== init sshd keys.'
    mkdir -p /run/sshd
    # ssh-keygen 初始化
    echo y | ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_rsa_key
    echo y | ssh-keygen -q -t ecdsa -N '' -f /etc/ssh/ssh_host_ecdsa_key
    echo y | ssh-keygen -q -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key
    echo ''
    echo "PasswordAuthentication yes" >> /etc/ssh/sshd_config

    if [[ -n "${USER:-}" ]] && [[ "$USER" != "root" ]]; then
        echo "======== init user: ${USER}."
        # 如果 USER 不是 root, 则创建用户
        if ! id -u "${USER}" >/dev/null 2>&1; then
            mkdir -p /home/"${USER}"
            groupadd --gid 1000 "${USER}"
            useradd  --uid 1000 --gid "${USER}" -d /home/"${USER}" -m -s /usr/bin/zsh "${USER}"
            echo "${USER} ALL=(root) NOPASSWD:ALL" > /etc/sudoers.d/"${USER}"
            chmod 0440 /etc/sudoers.d/"${USER}" && chmod g+rw /home
            cp /root/.zshrc /home/"${USER}"/.zshrc
            sed -i "s#\$HOME#/root#g" /home/"${USER}"/.zshrc
            chown -R "${USER}":"${USER}" /home/"${USER}"
            # chmod 777 -R /root/.nvm && chmod 777 -R /root/.sdkman
        fi
        # 配置 ssh 的登录密码
        if [[ -n "${PASSWORD}" ]]; then
            echo "set ssh password for ${USER} ..."
            chpasswd <<<"${USER}:${PASSWORD}"
        fi
    fi
    # 配置 root 用户的 ssh password
    if [[ -n "${PASSROOT}" ]]; then
        echo 'set ssh password for root ...'
        chpasswd <<<"root:${PASSROOT}"
        echo "PermitRootLogin yes" >> /etc/ssh/sshd_config
    fi
fi

# fix-sysenv
# 将 Go 环境变量写入 /etc/zsh/zshenv，所有 zsh 登录即生效
ZSHENV="/etc/zsh/zshenv"
mkdir -p /etc/zsh

# 仅当 /usr/local/golang 存在时才写入，避免无 Go 环境时污染 PATH
if [ -d /usr/local/golang ]; then
    # 避免重复写入
    if ! grep -q 'GOROOT' "${ZSHENV}" 2>/dev/null; then
        cat >> "${ZSHENV}" <<'EOF'

if [ -z "$GOROOT" ]
then
	export GOROOT=/usr/local/golang
	export GOPATH=~/.go
	export PATH="$PATH:$GOROOT/bin:$GOPATH/bin"
fi
EOF
    fi
fi

## =======================================================================

echo "======== end for init script."