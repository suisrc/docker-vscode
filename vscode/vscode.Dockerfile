# 推荐一个最小安装版, alpine无法运行vsc的node
# FROM alpine:3
# FROM debian:buster-slim
FROM ubuntu:focal

ARG VSC_DOMAIN=https://github.com/gitpod-io/openvscode-server/releases
ARG VSC_RELEASE=v1.65.2
ARG VSC_URL=${VSC_DOMAIN}/download/openvscode-server-${VSC_RELEASE}/openvscode-server-${VSC_RELEASE}-linux-x64.tar.gz
ARG VSC_HOME=/vsc

ARG S6_RELEASE=v3.1.0.1
ARG S6_APP=https://github.com/just-containers/s6-overlay/releases/download/${S6_RELEASE}/s6-overlay-x86_64.tar.xz
ARG S6_CFG=https://github.com/just-containers/s6-overlay/releases/download/${S6_RELEASE}/s6-overlay-noarch.tar.xz


ARG OH_MY_ZSH_SH_URL
ARG OH_MY_ZSH_SUGGES

# linux and softs
# apk add --no-cache openssh bash vim curl jq tar git #apline软件
# dumb-init #使用s6代替
RUN apt update && apt install --no-install-recommends -y \
        sudo ca-certificates curl git procps jq bash net-tools zsh vim nano ntpdate locales openssh-client xz-utils libatomic1 &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# locale & language
RUN sed -i "s/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/g" /etc/locale.gen && locale-gen
ENV LC_ALL=zh_CN.UTF-8 \
    SHELL=/bin/bash

# s6-overlay
RUN curl -o /tmp/s6-cfg.tar.xz -L "${S6_CFG}" && tar -C / -Jxpf /tmp/s6-cfg.tar.xz &&\
    curl -o /tmp/s6-app.tar.xz -L "${S6_APP}" && tar -C / -Jxpf /tmp/s6-app.tar.xz &&\
    rm -rf /tmp/*
    #tar xzf /tmp/s6.tar.gz -C / --exclude='./bin' && tar xzf /tmp/s6.tar.gz -C /usr ./bin &&\

COPY init-git.sh /command/git
COPY init-ssh.sh /command/ssh

# https://github.com/just-containers/s6-overlay
ENV S6_KEEP_ENV=true
ENTRYPOINT ["/init"]

# Creating the user and usergroup
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID

RUN groupadd --gid $USER_GID $USERNAME \
    && useradd --uid $USER_UID --gid $USERNAME -m -s /bin/bash $USERNAME \
    && echo $USERNAME ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/$USERNAME \
    && chmod 0440 /etc/sudoers.d/$USERNAME

RUN chmod g+rw /home && \
    mkdir -p /home/workspace && \
    mkdir -p ${VSC_HOME} && \
    chown -R $USERNAME:$USERNAME /home/workspace && \
    chown -R $USERNAME:$USERNAME ${VSC_HOME}

USER $USERNAME

# install oh-my-zsh
# https://raw.githubusercontent.com/robbyrussell/oh-my-zsh/master/tools/install.sh => https://gitee.com/mirrors/oh-my-zsh/raw/master/tools/install.sh
# https://github.com/zsh-users/zsh-autosuggestions => https://gitee.com/ncr/zsh-autosuggestions
RUN if [ -z ${OH_MY_ZSH_SH_URL+x} ]; then \
        OH_MY_ZSH_SH_URL="https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh"; \
    fi &&\
    if [ -z ${OH_MY_ZSH_SUGGES+x} ]; then \
        OH_MY_ZSH_SUGGES="https://github.com/zsh-users/zsh-autosuggestions"; \
    fi &&\
    sh -c "$(curl -fsSL ${OH_MY_ZSH_SH_URL})" &&\
    git clone "${OH_MY_ZSH_SUGGES}" ~/.oh-my-zsh/plugins/zsh-autosuggestions &&\
    echo "source ~/.oh-my-zsh/plugins/zsh-autosuggestions/zsh-autosuggestions.zsh" >> ~/.zshrc
    #sed -i "s/ZSH_THEME=\"robbyrussell\"/ZSH_THEME=\"agnoster\"/g" ~/.zshrc

USER root

# vscode-server
RUN if [ -z ${VSC_URL+x} ]; then \
        if [ -z ${VSC_RELEASE+x} ]; then \
            VSC_RELEASE=$(curl -sX GET "${VSC_DOMAIN}/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        VSC_URL=$(curl -sX GET "${VSC_DOMAIN}/tags/${VSC_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("-linux-x64.tar.gz")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/vsc.tar.gz -L "${VSC_URL}" && mkdir -p ${VSC_HOME} && tar xzf /tmp/vsc.tar.gz -C ${VSC_HOME}/ --strip-components=1 && \
    ln -s ${VSC_HOME}/bin/openvscode-server /usr/bin/vscode-server &&\
    cp ${VSC_HOME}/bin/remote-cli/openvscode-server ${VSC_HOME}/bin/remote-cli/vscode &&\
    sed -i 's/"$0"/"$(readlink -f $0)"/' ${VSC_HOME}/bin/remote-cli/vscode &&\
    ln -s ${VSC_HOME}/bin/remote-cli/vscode /usr/bin/vscode &&\
    rm -rf /tmp/*

# config s6
COPY s6-git.sh   /etc/cont-init.d/git-init
COPY s6-vsc.sh   /etc/services.d/vscode/run

WORKDIR /home/worksapce
ENV     HOME=/home/worksapce
#VOLUME [ "/home/worksapce" ]

EXPOSE 7000

USER $USERNAME

# install extension
RUN vscode --install-extension mhutchie.git-graph &&\
    vscode --install-extension esbenp.prettier-vscode &&\
    vscode --install-extension humao.rest-client
    #vscode --install-extension ms-ceintl.vscode-language-pack-zh-hans

# config for user
COPY locale.json    ~/.local/share/code-server/User/locale.json
COPY settings2.json ~/.local/share/code-server/User/settings.json
