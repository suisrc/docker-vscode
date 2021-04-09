# 推荐一个最小安装版, alpine无法运行vsc的node
# FROM alpine:3
FROM debian:buster-slim

#ARG CODE_RELEASE=v1.52.1
#ARG CODE_URL=https://github.com/suisrc/code-server/releases/download/${CODE_RELEASE}/code-server-linux-amd64.tar.gz
ARG CODE_URL=https://github.com/cdr/code-server/releases/download/v3.9.3/code-server-3.9.3-linux-amd64.tar.gz

ARG S6_RELEASE=v2.2.0.3
ARG S6_URL=https://github.com/just-containers/s6-overlay/releases/download/${S6_RELEASE}/s6-overlay-amd64.tar.gz

# linux and softs
# apk add --no-cache openssh bash vim curl jq tar git #apline软件
# dumb-init #使用s6代替
RUN apt update && apt install --no-install-recommends -y \
        sudo ca-certificates curl git procps jq bash net-tools vim nano ntpdate locales &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# s6-overlay
RUN curl -o /tmp/s6.tar.gz -L "${S6_URL}" &&\
    tar xzf /tmp/s6.tar.gz -C / &&\
    rm -rf /tmp/*

# code-server
# 默认使用cdr/code-server的应用
RUN if [ -z ${CODE_URL+x} ]; then \
        if [ -z ${CODE_RELEASE+x} ]; then \
            CODE_RELEASE=$(curl -sX GET "https://api.github.com/repos/cdr/code-server/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        CODE_URL=$(curl -sX GET "https://api.github.com/repos/cdr/code-server/releases/tags/${CODE_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("linux-amd64.tar.gz")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/code.tar.gz -L "${CODE_URL}" && \
    mkdir -p /usr/lib/code-server &&\
    tar xzf /tmp/code.tar.gz -C /usr/lib/code-server/ --strip-components=1 && \
    ln -s /usr/lib/code-server/bin/code-server /usr/bin/code-server &&\
    rm -rf /tmp/*

# change code server extension store
# 更改默认的应用市场位微软的应用市场
ENV SERVICE_URL=https://marketplace.visualstudio.com/_apis/public/gallery \
    ITEM_URL=https://marketplace.visualstudio.com/items

# install code server extension
RUN code-server --install-extension ms-ceintl.vscode-language-pack-zh-hans &&\
    code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode &&\
    code-server --install-extension humao.rest-client

# config for user
COPY ["locale.json", "settings2.json", "/root/.local/share/code-server/User/"]

# locale & language
RUN sed -i "s/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/g" /etc/locale.gen && locale-gen
ENV LC_ALL=zh_CN.UTF-8 \
    SHELL=/bin/bash

# worksapce
# 测试过程中发现，如果使用root账户，会导致程序部分插件没有访问User/文件夹的权限
RUN mv /root/.local/share/code-server/User/settings2.json /root/.local/share/code-server/User/settings.json &&\
    mkdir -p /home/project && mkdir -p /sh && \
    mkdir -p /root/.local/share/code-server/User/globalStorage

COPY init-git.sh /sh/git
COPY init-ssh.sh /sh/ssh

WORKDIR /home/project
#VOLUME [ "/home/project" ]

EXPOSE 7000

# code-server start
#ENTRYPOINT ["entrypoint.sh"]
#CMD [ "code-server", "--bind-addr", "0.0.0.0:7000", "--disable-telemetry", "--disable-updates", "/home/project"]

# https://github.com/just-containers/s6-overlay
ENTRYPOINT ["/init"]

# /etc/services.d/
RUN mkdir -p /etc/services.d/vscode && \
    echo "#!/usr/bin/execlineb -P\ncode-server --bind-addr 0.0.0.0:7000 --disable-telemetry --disable-update-check /home/project" > /etc/services.d/vscode/run && \
    chmod +x /etc/services.d/vscode/run &&\
    #echo "#!/usr/bin/execlineb -S1\ns6-svscanctl -t /var/run/s6/services" > /etc/services.d/vscode/finish && \
    #chmod +x /etc/services.d/vscode/finish &&\
    echo "#!/usr/bin/execlineb -P\n/sh/git" > /etc/cont-init.d/git-init &&\
    chmod +x /etc/cont-init.d/git-init
ENV S6_KEEP_ENV=true

# /etc/cont-init.d/
# /etc/fix-attrs.d/
#RUN echo "#!/usr/bin/execlineb -P\n/git-init.sh"         > /etc/cont-init.d/git-init &&\
#    echo "/etc/cont-init.d/git-init true root 0755 0755" > /etc/fix-attrs.d/git-init
