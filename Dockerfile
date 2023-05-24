FROM suisrc/openresty:1.21.4.1-hp-3 as openresty

# Path: debian/Dockerfile
FROM debian:bullseye

LABEL maintainer="suisrc@outlook.com"

ARG S6_RELEASE=3.1.4.1 \
    VNC_RELEASE=1.1.0

# copy openresty resource
COPY --from=openresty /usr/local/openresty /usr/local/openresty
COPY --from=openresty /etc/nginx /etc/nginx
# COPY --from=openresty /var/run/openresty /var/run/openresty
# COPY --from=openresty /www /www -> /usr/local/openresty/nginx/html/

# update linux
RUN apt update && \
    echo "**** install base module ****" && \
    DEBIAN_FRONTEND=noninteractive \
    apt install --no-install-recommends -y \
    autoclean \
    bash \
    binutils \
    ca-certificates \
    curl \
    dpkg \
    fontconfig \
    fonts-noto-core \
    fonts-noto-cjk \
    fonts-noto-color-emoji \
    gettext \
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
    libz-dev \
    zlib1g-dev \
    && \
    echo "**** fonts tweaks ****" && \
    sed -i "s/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/g" /etc/locale.gen && locale-gen &&\
    fc-cache -fv &&\
    echo "**** install kde module ****" && \
    DEBIAN_FRONTEND=noninteractive \
    apt install --no-install-recommends -y \
    dolphin \
    gwenview \
    kde-config-gtk-style \
    kdialog \
    kfind \
    khotkeys \
    kio-extras \
    knewstuff-dialog \
    konsole \
    ksysguard \
    kwin-addons \
    kwin-x11 \
    kwrite \
    plasma-desktop \
    plasma-workspace \
    qml-module-qt-labs-platform \
    systemsettings \
    pulseaudio \
    pulseaudio-utils \
    && \
    echo "**** desktop tweaks ****" && \
    mkdir /var/run/openresty && ln -s /usr/local/openresty/nginx/html /www && \
    sed -i \
    's/applications:org.kde.discover.desktop,/applications:org.kde.konsole.desktop,/g' \
    /usr/share/plasma/plasmoids/org.kde.plasma.taskmanager/contents/config/main.xml && \
    echo "**** cleanup ****" && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# fcitx5 fcitx5-pinyin??

# Creating the user and usergroup
ARG USERNAME=wsc
RUN groupadd --gid 1000 $USERNAME && \
    useradd  --uid 1000 --gid $USERNAME -m -s /bin/bash $USERNAME && \
    echo $USERNAME ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/$USERNAME && \
    echo "$USERNAME:abc123" | chpasswd && usermod -aG sudo $USERNAME && \
    mkdir /home/$USERNAME && chown 1000:1000 /home/$USERNAME && \
    chmod 0440 /etc/sudoers.d/$USERNAME && chmod g+rw /home

# install KasmVNC
# https://github.com/kasmtech/KasmVNC/releases
RUN VNC_URL="https://github.com/kasmtech/KasmVNC/releases/download/v${VNC_RELEASE}/kasmvncserver_bullseye_${VNC_RELEASE}_amd64.deb"
    curl -o /tmp/kasmvncserver.deb -L "${VNC_URL}" && \
    apt install -y /tmp/kasmvncserver.deb && \
    ln -s /usr/local/share/kasmvnc /usr/share/kasmvnc && \
    ln -s /usr/local/etc/kasmvnc /etc/kasmvnc && \
    ln -s /usr/local/lib/kasmvnc /usr/lib/kasmvncserver && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# s6-overlay
# https://github.com/just-containers/s6-overlay/releases
RUN S6_RURL="https://github.com/just-containers/s6-overlay/releases" &&\
    S6_APP="${S6_RURL}/download/v${S6_RELEASE}/s6-overlay-x86_64.tar.xz" &&\
    S6_CFG="${S6_RURL}/download/v${S6_RELEASE}/s6-overlay-noarch.tar.xz" &&\
    curl -o /tmp/s6-cfg.tar.xz -L "${S6_CFG}" && tar -C / -Jxpf /tmp/s6-cfg.tar.xz &&\
    curl -o /tmp/s6-app.tar.xz -L "${S6_APP}" && tar -C / -Jxpf /tmp/s6-app.tar.xz &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

ENV HOME=/home/$USERNAME  \
    S6_KEEP_ENV=true \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0

COPY /root/ /

WORKDIR    /home/$USERNAME
ENTRYPOINT ["/init"]

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


# install msedge
# ??替代 apt install chromium chromium-sandbox
RUN if [ -z ${EDGE_BUILD+x} ]; then \
        EDGE_RELEASE=$(curl -q https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/ | grep href | grep .deb | sed 's/.*href="//g'  | cut -d '"' -f1 | sort --version-sort | tail -1); \
    fi &&\
    EDGE_RURL="https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/$EDGE_RELEASE" &&\
    curl -o /tmp/msedge.deb -L "${EDGE_RURL}" &&\
    apt install -y /tmp/msedge.deb &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# sed -i 's|"\$@"| --no-sandbox  &|' /opt/microsoft/msedge/microsoft-edge
# cp /usr/share/applications/microsoft-edge.desktop $HOME/Desktop/msedge.desktop

# install vscode
# ??替代  https://github.com/VSCodium/vscodium/releases/download/1.78.2.23132/codium_1.78.2.23132_amd64.deb
RUN CODE_RURL="vscode.deb https://update.code.visualstudio.com/latest/linux-deb-x64/stable"
    curl -o /tmp/vscode.deb -L "${CODE_RURL}" &&\
    apt install -y /tmp/vscode.deb &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# sed -i 's#/usr/share/code/code#& --no-sandbox##' /usr/share/applications/code.desktop
# cp /usr/share/applications/code.desktop $HOME/Desktop/vscode.desktop

# USER $USERNAME
