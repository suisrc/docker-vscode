# FROM debian:stretch-slim
FROM debian:buster-slim
#FROM debian:buster

# args
#ARG CODE_RELEASE=v1.52.1
#ARG CODE_URL=https://github.com/suisrc/code-server/releases/download/${CODE_RELEASE}/code-server-linux-amd64.tar.gz
ARG CODE_URL=https://github.com/cdr/code-server/releases/download/v3.9.3/code-server-3.9.3-linux-amd64.tar.gz

ARG S6_RELEASE=v2.2.0.3
ARG S6_URL=https://github.com/just-containers/s6-overlay/releases/download/${S6_RELEASE}/s6-overlay-amd64.tar.gz

ARG FONT_URL
ARG FONT_RELEASE

ARG OH_MY_ZSH_SH_URL
ARG OH_MY_ZSH_SUGGES

ARG LINUX_MIRRORS
#ARG LINUX_MIRRORS=http://mirrors.aliyun.com

# set version label
LABEL maintainer="suisrc@outlook.com"

ENV container docker
# update linux
RUN if [ ! -z ${LINUX_MIRRORS+x} ]; then \
        mv /etc/apt/sources.list /etc/apt/sources.list.bak && \
        echo "deb ${LINUX_MIRRORS}/debian/ buster main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian/ buster main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb ${LINUX_MIRRORS}/debian-security buster/updates main" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian-security buster/updates main" >>/etc/apt/sources.list &&\
        echo "deb ${LINUX_MIRRORS}/debian/ buster-updates main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian/ buster-updates main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb ${LINUX_MIRRORS}/debian/ buster-backports main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian/ buster-backports main non-free contrib" >>/etc/apt/sources.list; \
    fi &&\
    apt update && \
    apt install --no-install-recommends -y \
        sudo ca-certificates curl git procps jq net-tools zsh vim p7zip nano fontconfig ntpdate locales dpkg openssh-server \
        gcc build-essential libz-dev zlib1g-dev &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# install sarasa-gothic
RUN if [ -z ${FONT_URL+x} ]; then \
        if [ -z ${FONT_RELEASE+x} ]; then \
            FONT_RELEASE=$(curl -sX GET "https://api.github.com/repos/suisrc/Sarasa-Gothic/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        FONT_URL=$(curl -sX GET "https://api.github.com/repos/suisrc/Sarasa-Gothic/releases/tags/${FONT_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("sc.7z")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/sarasa-gothic-ttf.7z -L "${FONT_URL}" && \
    mkdir -p /usr/share/fonts/truetype/sarasa-gothic &&\
    cd /usr/share/fonts/truetype/sarasa-gothic &&\
    p7zip --uncompress /tmp/sarasa-gothic-ttf.7z &&\
    fc-cache -f -v &&\
    rm -rf /tmp/*

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
    git clone "${OH_MY_ZSH_SUGGES}" /root/.oh-my-zsh/plugins/zsh-autosuggestions &&\
    echo "source ~/.oh-my-zsh/plugins/zsh-autosuggestions/zsh-autosuggestions.zsh" >> /root/.zshrc
    #sed -i "s/ZSH_THEME=\"robbyrussell\"/ZSH_THEME=\"agnoster\"/g" /root/.zshrc

# s6-overlay
RUN curl -o /tmp/s6.tar.gz -L "${S6_URL}" &&\
    tar xzf /tmp/s6.tar.gz -C / &&\
    rm -rf /tmp/*

# install code-server
# tar xzf /tmp/code.tar.gz -C /usr/local/bin/ --strip-components=1 --wildcards code-server*/code-server && \
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

# install code server extension
ENV SERVICE_URL=https://marketplace.visualstudio.com/_apis/public/gallery \
    ITEM_URL=https://marketplace.visualstudio.com/items

# install code-server extension
RUN code-server --install-extension ms-ceintl.vscode-language-pack-zh-hans &&\
    code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode

# config for user
COPY ["settings.json", "locale.json", "/root/.local/share/code-server/User/"]

# locale & language
RUN sed -i "s/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/g" /etc/locale.gen && locale-gen
ENV LC_ALL=zh_CN.UTF-8 \
    SHELL=/bin/zsh


# worksapce
# 测试过程中发现，如果使用root账户，会导致程序部分插件没有访问User/文件夹的权限
RUN mkdir -p /home/project && mkdir -p /home/test/mirror && mkdir -p /sh/ &&\
    mkdir -p /root/.local/share/code-server/User/globalStorage
# test
COPY init-git.sh /sh/git
COPY init-ssh.sh /sh/ssh
COPY test.*   /home/test/
COPY mirror-* /home/test/mirror/

WORKDIR /home/project
#VOLUME [ "/home/project" ]

# code-server start
EXPOSE 7000
ENTRYPOINT ["/init"]

RUN mkdir -p /etc/services.d/vscode && \
    echo "#!/usr/bin/execlineb -P\ncode-server --bind-addr 0.0.0.0:7000 --disable-telemetry --disable-update-check /home/project" > /etc/services.d/vscode/run && \
    chmod +x /etc/services.d/vscode/run &&\
    #echo "#!/usr/bin/execlineb -S1\ns6-svscanctl -t /var/run/s6/services" > /etc/services.d/vscode/finish && \
    #chmod +x /etc/services.d/vscode/finish &&\
    echo "#!/usr/bin/execlineb -P\n/sh/git" > /etc/cont-init.d/git-init &&\
    chmod +x /etc/cont-init.d/git-init
ENV S6_KEEP_ENV=true
