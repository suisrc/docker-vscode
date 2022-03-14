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

# linux and softs
# apk add --no-cache openssh bash vim curl jq tar git #apline软件
# dumb-init #使用s6代替
RUN apt update && apt install --no-install-recommends -y \
    sudo ca-certificates curl git procps jq bash net-tools iputils-ping zsh vim nano ntpdate locales openssh-client xz-utils libatomic1 &&\
    sed -i "s/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/g" /etc/locale.gen && locale-gen &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# s6-overlay
RUN curl -o /tmp/s6-cfg.tar.xz -L "${S6_CFG}" && tar -C / -Jxpf /tmp/s6-cfg.tar.xz &&\
    curl -o /tmp/s6-app.tar.xz -L "${S6_APP}" && tar -C / -Jxpf /tmp/s6-app.tar.xz &&\
    rm -rf /tmp/*
    #tar xzf /tmp/s6.tar.gz -C / --exclude='./bin' && tar xzf /tmp/s6.tar.gz -C /usr ./bin

COPY init-* /command/
# config s6
COPY s6-git /etc/cont-init.d/git-init
COPY s6-vsc /etc/services.d/vscode/run

# https://github.com/just-containers/s6-overlay
ENTRYPOINT ["/init"]

ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    S6_KEEP_ENV=true

# Creating the user and usergroup
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID

RUN groupadd --gid $USER_GID $USERNAME && \
    useradd --uid $USER_UID --gid $USERNAME -m -s /bin/bash $USERNAME   && \
    echo $USERNAME ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/$USERNAME && \
    chmod 0440 /etc/sudoers.d/$USERNAME && \
    chmod g+rw /home && \
    mkdir -p   /workspace  && \
    mkdir -p   ${VSC_HOME} && \
    chown -R   $USERNAME:$USERNAME /workspace  && \
    chown -R   $USERNAME:$USERNAME ${VSC_HOME} && \
    ln    -s   /workspace /ws

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
    ln -s ${VSC_HOME}/bin/openvscode-server /usr/bin/code-server &&\
    cp ${VSC_HOME}/bin/remote-cli/openvscode-server ${VSC_HOME}/bin/remote-cli/code &&\
    sed -i 's/"$0"/"$(readlink -f $0)"/' ${VSC_HOME}/bin/remote-cli/code &&\
    ln -s ${VSC_HOME}/bin/remote-cli/code /usr/bin/code &&\
    rm -rf /tmp/*

# =============================================================================================
USER $USERNAME

# install oh-my-zsh
#ARG OH_MY_ZSH_SH_URL=https://gitee.com/mirrors/oh-my-zsh/raw/master/tools/install.sh
#ARG OH_MY_ZSH_SUGGES=https://gitee.com/ncr/zsh-autosuggestions
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

ARG USERDATA=/home/$USERNAME/.openvscode-server/data
# install extension ?ms-ceintl.vscode-language-pack-zh-hans
RUN code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode &&\
    code-server --install-extension humao.rest-client &&\
    mkdir -p $USERDATA/Machine

# config for user or machine
COPY locale.json    $USERDATA/Machine/locale.json
COPY settings2.json $USERDATA/Machine/settings.json

# =============================================================================================
#EXPOSE 7000
WORKDIR /workspace