# 推荐一个最下安装版, alpine无法运行vsc的node
# FROM alpine:3
FROM debian:buster-slim

ARG CODE_URL
ARG CODE_RELEASE

# linux and softs
RUN echo "**** update linux ****" && \
    # apk add --no-cache openssh bash vim curl jq tar git
    apt update && apt install --no-install-recommends -y \
        dumb-init sudo ca-certificates curl git jq bash net-tools vim nano ntpdate locales &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# Code-Server
RUN echo "**** install code-server ****" && \
    if [ -z ${CODE_URL+x} ]; then \
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
    ln -s /usr/lib/code-server/code-server /usr/bin/code-server &&\
    rm -rf /tmp/*

# install code server extension
ENV SERVICE_URL=https://marketplace.visualstudio.com/_apis/public/gallery \
    ITEM_URL=https://marketplace.visualstudio.com/items

RUN echo "**** install code-server extension ****" && \
    code-server --install-extension ms-ceintl.vscode-language-pack-zh-hans &&\
    code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode 

# config for user
COPY ["locale.json", "settings2.json", "/root/.local/share/code-server/User/"]

# locale & language
RUN sed -i "s/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/g" /etc/locale.gen && locale-gen
ENV LC_ALL=zh_CN.UTF-8 \
    SHELL=/bin/bash

COPY entrypoint.sh /usr/local/bin/

# worksapce
# 测试过程中发现，如果使用root账户，会导致程序部分插件没有访问User/文件夹的权限
RUN mv /root/.local/share/code-server/User/settings2.json /root/.local/share/code-server/User/settings.json &&\
    mkdir -p /home/project && \
    chmod +x /usr/local/bin/entrypoint.sh &&\
    mkdir -p /root/.local/share/code-server/User/globalStorage

WORKDIR  /home/project
#VOLUME [ "/home/project" ]

# code-server start
EXPOSE 7000
ENTRYPOINT ["dumb-init", "entrypoint.sh"]
CMD [ "code-server", "--bind-addr", "0.0.0.0:7000", "--disable-telemetry", "--disable-updates", "/home/project"]

