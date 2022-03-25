# 最小安装版, 这是一个工具，不能作为开发环境的基础环境使用，因此这里只提供root身份
FROM node:14-alpine

LABEL maintainer="suisrc@outlook.com"

ARG VSC_RURL=https://github.com/coder/code-server/releases
ARG VSC_RELEASE=4.2.0
ARG VSC_URL=${VSC_RURL}/download/v${VSC_RELEASE}/code-server-${VSC_RELEASE}-linux-amd64.tar.gz
ARG VSC_HOME=/vsc

ARG S6_RURL=https://github.com/just-containers/s6-overlay/releases
ARG S6_RELEASE=v3.1.0.1
ARG S6_APP=$S6_RURL/download/${S6_RELEASE}/s6-overlay-x86_64.tar.xz
ARG S6_CFG=$S6_RURL/download/${S6_RELEASE}/s6-overlay-noarch.tar.xz

# linux and softs
RUN apk add --no-cache curl gnupg openssh bash zsh vim jq tar git xz libc6-compat &&\
    rm -rf /tmp/* /var/tmp/*

# =============================================================================================
# s6-overlay
RUN curl -o /tmp/s6-cfg.tar.xz -L "${S6_CFG}" && tar -C / -Jxpf /tmp/s6-cfg.tar.xz &&\
    curl -o /tmp/s6-app.tar.xz -L "${S6_APP}" && tar -C / -Jxpf /tmp/s6-app.tar.xz &&\
    rm -rf  /tmp/*
    #tar xzf /tmp/s6.tar.gz -C / --exclude='./bin' && tar xzf /tmp/s6.tar.gz -C /usr ./bin

COPY init-* /command/
# config s6
COPY s6-init /etc/cont-init.d/vscs
COPY s6-vscs /etc/services.d/vscs/run
# copy demo
COPY test.*   /home/test/demo/
COPY mirror-* /home/test/mirror/
# copy kubectl
COPY kubectl-*  /usr/local/bin/

ARG USERDATA=/workspace/.local/share/code-server
RUN mkdir /workspace && ln -s /workspace /ws && mkdir -p ${VSC_HOME}
COPY settings1.json /workspace/.vscode/settings.json

# https://github.com/just-containers/s6-overlay
WORKDIR   /workspace
ENTRYPOINT ["/init"]

ENV HOME=/workspace  \
    S6_KEEP_ENV=true \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0

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
    echo "source ~/.oh-my-zsh/plugins/zsh-autosuggestions/zsh-autosuggestions.zsh" >> ~/.zshrc &&\
    sed -i "1iZSH_DISABLE_COMPFIX=true" ~/.zshrc
    #sed -i "s/ZSH_THEME=\"robbyrussell\"/ZSH_THEME=\"agnoster\"/g" ~/.zshrc

# =============================================================================================
# vscode-server
RUN if [ -z ${VSC_URL+x} ]; then \
        if [ -z ${VSC_RELEASE+x} ]; then \
            VSC_RELEASE=$(curl -sX GET "${VSC_RURL}/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        VSC_URL=$(curl -sX GET "${VSC_RURL}/tags/${VSC_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("-linux-x64.tar.gz")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/vsc.tar.gz -L "${VSC_URL}" && mkdir -p ${VSC_HOME} && tar xzf /tmp/vsc.tar.gz -C ${VSC_HOME}/ --strip-components=1 && \
    ln -s ${VSC_HOME}/bin/code-server /usr/bin/code-server &&\
    rm -f ${VSC_HOME}/node            && ln -s /usr/local/bin/node ${VSC_HOME}/node &&\
    rm -f ${VSC_HOME}/lib/node        && ln -s /usr/local/bin/node ${VSC_HOME}/lib/node &&\
    rm -f ${VSC_HOME}/lib/vscode/node && ln -s /usr/local/bin/node ${VSC_HOME}/lib/vscode/node &&\
    ln -s /lib/ld-musl-x86_64.so.1 /lib/ld-linux-x86-64.so.2 &&\
    rm -rf /tmp/*

ENV EXTENSIONS=""

# =============================================================================================
# install extension ?ms-ceintl.vscode-language-pack-zh-hans
RUN code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode &&\
    code-server --install-extension humao.rest-client &&\
    rm -rf $USERDATA/CachedExtensionVSIXs/*
# config for user or machine
COPY locale.json    $USERDATA/Machine/locale.json
COPY settings2.json $USERDATA/Machine/settings.json

#EXPOSE 7000