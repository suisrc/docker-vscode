FROM suisrc/openresty:1.21.4.1-hu-3 as openresty


######### Build Container Image ###########
FROM kasmweb/core-ubuntu-jammy:1.13.1

ARG S6_RELEASE=3.1.5.0

LABEL maintainer="suisrc@outlook.com"
######### Start Customizations ###########
USER root

ENV HOME /home/kasm-default-profile
ENV STARTUPDIR /dockerstartup
ENV INST_SCRIPTS $STARTUPDIR/install
WORKDIR $HOME

######### Customize Container Here ###########

# copy openresty resource
COPY --from=openresty /usr/local/openresty /usr/local/openresty
COPY --from=openresty /etc/nginx /etc/nginx
# COPY --from=openresty /var/run/openresty /var/run/openresty
# COPY --from=openresty /www /www -> /usr/local/openresty/nginx/html/

RUN ln -s /usr/local/openresty/nginx/sbin/nginx /usr/bin/nginx && \
    ln -s /usr/local/openresty/nginx/html /www && mkdir /var/run/openresty

# copy resource
COPY /root/ /

# update linux
RUN apt update && \
    DEBIAN_FRONTEND=noninteractive \
    apt install --no-install-recommends -y \
    binutils \
    ca-certificates \
    curl \
    dpkg \
    gcc \
    git \
    inotify-tools \
    iputils-ping \
    jq \
    libatomic1 \
    libxfont2 \
    locales \
    nano \
    net-tools \
    ntpdate \
    procps \
    ssl-cert \
    openssh-server \
    p7zip \
    unzip \
    xz-utils \
    sudo \
    libdbus-glib-1-2 \
    # build-essential \
    # python3-dev \
    # python3-pip \
    # python3-venv \
    # libz-dev \
    # zlib1g-dev \
    && \
    # pip3 install --upgrade pip && \
    echo kasm-user ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/kasm-user && \
    apt autoremove -y && apt autoclean -y && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# s6-overlay
# https://github.com/just-containers/s6-overlay/releases
RUN S6_RURL="https://github.com/just-containers/s6-overlay/releases" &&\
    S6_APP="${S6_RURL}/download/v${S6_RELEASE}/s6-overlay-x86_64.tar.xz" &&\
    S6_CFG="${S6_RURL}/download/v${S6_RELEASE}/s6-overlay-noarch.tar.xz" &&\
    curl -o /tmp/s6-cfg.tar.xz -L "${S6_CFG}" && tar -C / -Jxpf /tmp/s6-cfg.tar.xz &&\
    curl -o /tmp/s6-app.tar.xz -L "${S6_APP}" && tar -C / -Jxpf /tmp/s6-app.tar.xz &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

ENV S6_KEEP_ENV=true \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0 \
    PATH="$PATH:/command"

# 安装 filebrowser
# 默认已经提供nginx进行文件下载，如果需要上传，可以启动 filebrowser
RUN FILE_URL="https://github.com/filebrowser/filebrowser/releases/download/v2.23.0/linux-amd64-filebrowser.tar.gz" &&\
    curl -o /tmp/filebrowser.tar.gz -L "${FILE_URL}" && tar -C /tmp -zxvf /tmp/filebrowser.tar.gz &&\
    mv /tmp/filebrowser /usr/local/bin/filebrowser &&\
    cp /etc/filebrowser/filesidecar.desktop $HOME/Desktop/filesr.desktop &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

## 安装 firefox
# https://download-installer.cdn.mozilla.net/pub/firefox/releases/115.0.3/linux-x86_64/en-US/firefox-115.0.3.tar.bz2
# https://download-installer.cdn.mozilla.net/pub/firefox/releases/115.0.3/linux-x86_64/zh-CN/firefox-115.0.3.tar.bz2
RUN FILE_URL="https://download-installer.cdn.mozilla.net/pub/firefox/releases/115.0.3/linux-x86_64/en-US/firefox-115.0.3.tar.bz2" &&\
    curl -o /tmp/firefox.tar.bz2 -L "${FILE_URL}" && tar -C /opt -jxvf /tmp/firefox.tar.bz2 &&\
    ln -s /opt/firefox/firefox /usr/local/bin/firefox &&\
    update-alternatives --install /usr/bin/x-www-browser x-www-browser /usr/local/bin/firefox 100 &&\
    update-alternatives --config x-www-browser &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# # 安装 msedge
# # ??替代 apt install chromium chromium-sandbox
# RUN if [ -z ${EDGE_RELEASE+x} ]; then \
#         EDGE_RELEASE=$(curl -q https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/ | grep href | grep .deb | sed 's/.*href="//g'  | cut -d '"' -f1 | sort --version-sort | tail -1); \
#     fi &&\
#     EDGE_URL="https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/$EDGE_RELEASE" &&\
#     curl -o /tmp/msedge.deb -L "${EDGE_URL}" &&\
#     apt update && apt install -y /tmp/msedge.deb &&\
#     cp /usr/share/applications/microsoft-edge.desktop $HOME/Desktop/msedge.desktop &&\
#     apt autoclean -y && \
#     rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# # 禁用沙盒
# # sed -i 's|"\$@"| --no-sandbox  &|' /opt/microsoft/msedge/microsoft-edge

# # # 安装 vscode
# # # ??替代  https://github.com/VSCodium/vscodium/releases/download/1.78.2.23132/codium_1.78.2.23132_amd64.deb
# RUN CODE_URL="https://update.code.visualstudio.com/latest/linux-deb-x64/stable" &&\
#     curl -o /tmp/vscode.deb -L "${CODE_URL}" &&\
#     apt update && apt install -y /tmp/vscode.deb &&\
#     cp /usr/share/applications/code.desktop $HOME/Desktop/vscode.desktop &&\
#     apt autoclean -y && \
#     rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# # 禁用沙盒
# # sed -i 's#/usr/share/code/code#& --no-sandbox##' /usr/share/applications/code.desktop

# https://github.com/kasmtech/workspaces-core-images/tree/release/1.13.1/src/common/startup_scripts
ENTRYPOINT ["/init"]
######### End Customizations ###########

RUN chown 1000:0 $HOME
RUN $STARTUPDIR/set_user_permission.sh $HOME

ENV KASM_USER kasm-user
ENV HOME /home/kasm-user
WORKDIR $HOME
RUN mkdir -p $HOME && chown -R 1000:0 $HOME

# 不可以迁移用户到1000上，使用s6-overlay在启动的时候，会因为权限问题无法正常启动
# USER 1000
