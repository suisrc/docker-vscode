FROM suisrc/openresty:1.21.4.1-hp-3 as openresty


######### Build Container Image ###########
FROM kasmweb/core-debian-bullseye:1.13.1:1.13.1

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
    openssh-server \
    p7zip \
    procps \
    ssl-cert \
    sudo \
    xz-utils \
    zsh \
    build-essential \
    python3-dev \
    python3-pip \
    python3-venv \
    libz-dev \
    zlib1g-dev \
    && \
    pip3 install --upgrade pip && \
    echo kasm-user ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/kasm-user && \
    apt autoclean -y && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# apt autoclean(删除安装包) vs autoremove(删除无效依赖和安装包)

# ubuntu字体没有问题，不需要安装，debian需要安装
#    fontconfig \
#    fonts-noto-core \
#    fonts-noto-cjk \
#    fonts-noto-color-emoji \
# 输入法可以使用主机输入法，不需要安装
#    fcitx5 fcitx5-pinyin

# install oh-my-zsh
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

# 安装 msedge
# ??替代 apt install chromium chromium-sandbox
RUN if [ -z ${EDGE_RELEASE+x} ]; then \
        EDGE_RELEASE=$(curl -q https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/ | grep href | grep .deb | sed 's/.*href="//g'  | cut -d '"' -f1 | sort --version-sort | tail -1); \
    fi &&\
    EDGE_URL="https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/$EDGE_RELEASE" &&\
    curl -o /tmp/msedge.deb -L "${EDGE_URL}" &&\
    apt update && apt install -y /tmp/msedge.deb &&\
    cp /usr/share/applications/microsoft-edge.desktop $HOME/Desktop/msedge.desktop &&\
    apt autoclean -y && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# 禁用沙盒
# sed -i 's|"\$@"| --no-sandbox  &|' /opt/microsoft/msedge/microsoft-edge

# # 安装 vscode
# # ??替代  https://github.com/VSCodium/vscodium/releases/download/1.78.2.23132/codium_1.78.2.23132_amd64.deb
RUN CODE_URL="https://update.code.visualstudio.com/latest/linux-deb-x64/stable" &&\
    curl -o /tmp/vscode.deb -L "${CODE_URL}" &&\
    apt update && apt install -y /tmp/vscode.deb &&\
    cp /usr/share/applications/code.desktop $HOME/Desktop/vscode.desktop &&\
    apt autoclean -y && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# 禁用沙盒
# sed -i 's#/usr/share/code/code#& --no-sandbox##' /usr/share/applications/code.desktop

# 重新定义启动脚本， 这只是一个demo
# 系统中保留了/dockerstartup/kasm_startup.sh脚本，没有定义, 是kasm官方预览
# custom_config.sh 也没有定义， 可以自己定义， 优先执行
# custom_startup.sh 在kasm_startup.sh之前执行

# https://github.com/kasmtech/workspaces-core-images/tree/release/1.13.1/src/common/startup_scripts
ENTRYPOINT ["/dockerstartup/os_startup.sh", \
            "/dockerstartup/kasm_default_profile.sh", \
            "/dockerstartup/vnc_startup.sh", \
            "/dockerstartup/kasm_startup.sh"]
######### End Customizations ###########

RUN chown 1000:0 $HOME
RUN $STARTUPDIR/set_user_permission.sh $HOME

ENV HOME /home/kasm-user
WORKDIR $HOME
RUN mkdir -p $HOME && chown -R 1000:0 $HOME

USER 1000
