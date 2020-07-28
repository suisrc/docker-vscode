# 推荐一个最小安装版, alpine无法运行vsc的node
# FROM alpine:3
FROM debian:buster-slim

ARG CODE_URL=https://github.com/suisrc/code-server/releases/download/v1.47.2/code-server-3.4.1-linux-amd64.tar.gz
ARG CODE_RELEASE

ARG S6_URL=https://github.com/just-containers/s6-overlay/releases/download/v2.0.0.1/s6-overlay-amd64.tar.gz

# linux and softs
# apk add --no-cache openssh bash vim curl jq tar git #apline软件
# dumb-init #使用s6代替
RUN apt update && apt install --no-install-recommends -y \
        sudo ca-certificates curl git jq bash net-tools vim nano ntpdate locales &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# s6-overlay
RUN curl -o /tmp/s6.tar.gz -L "${S6_URL}" && \ 
    tar xzf /tmp/s6.tar.gz -C / --exclude='./bin' && tar xzf /tmp/s6.tar.gz -C /usr ./bin &&\
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

WORKDIR /home/project
#VOLUME [ "/home/project" ]

EXPOSE 7000

# code-server start
#ENTRYPOINT ["entrypoint.sh"]
#CMD [ "code-server", "--bind-addr", "0.0.0.0:7000", "--disable-telemetry", "--disable-updates", "/home/project"]

# https://github.com/just-containers/s6-overlay
ENTRYPOINT ["/init"]
# /etc/fix-attrs.d/ 
# /etc/services.d/
# /etc/cont-init.d/
RUN mkdir -p /etc/services.d/code-server && \
    echo "#!/usr/bin/execlineb -P\ncode-server --bind-addr 0.0.0.0:7000 --disable-telemetry --disable-updates /home/project" > /etc/services.d/code-server/run && \
    chmod +x /etc/services.d/code-server/run


