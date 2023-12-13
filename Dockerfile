# syntax=docker/dockerfile:1
# empty image=scratch

# ================================================================
# https://github.com/linuxserver/docker-baseimage-kasmvnc/pkgs/container/baseimage-kasmvnc
# FROM ghcr.io/linuxserver/baseimage-kasmvnc:arch-version-2023-12-12 as kasm-stage

# nodejs builder
FROM ubuntu:jammy as kclient-stage

RUN echo "**** install build deps ****" && \
    apt-get update && apt-get install -y gnupg && \
    curl -s https://deb.nodesource.com/gpgkey/nodesource.gpg.key | apt-key add - && \
    echo 'deb https://deb.nodesource.com/node_18.x jammy main' > /etc/apt/sources.list.d/nodesource.list && \
    apt-get update && apt-get install -y \
    g++ \
    gcc \
    libpam0g-dev \
    libpulse-dev \
    make \
    nodejs &&\
    echo "**** grab source ****" && \
    mkdir -p /kclient && \
    curl -o /tmp/kclient.tar.gz -L "https://github.com/linuxserver/kclient/archive/refs/tags/0.3.6.tar.gz" && \
    tar xf /tmp/kclient.tar.gz -C /kclient/ --strip-components=1 &&\
    echo "**** install node modules ****" && \
    cd /kclient && \
    npm install && \
    rm -f package-lock.json

# ================================================================
FROM suisrc/openresty:1.21.4.1-hu-3 as openresty

# ================================================================
FROM alpine:3.17 as rootfs-stage

# environment
ENV REL=jammy
ENV ARCH=amd64

# install packages
RUN apk add --no-cache bash curl tzdata xz

# grab base image
RUN mkdir /out && \
  curl -o /rootfs.tar.gz -L https://partner-images.canonical.com/core/${REL}/current/ubuntu-${REL}-core-cloudimg-${ARCH}-root.tar.gz && \
  tar xf  /rootfs.tar.gz -C /out && \
  rm -rf  /out/var/log/*

# https://github.com/just-containers/s6-overlay
# set version for s6 overlay
ARG S6_OVERLAY_VERSION="3.1.6.2"
ARG S6_OVERLAY_ARCH="x86_64"

# add s6 overlay
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp
RUN tar -C /out -Jxpf /tmp/s6-overlay-noarch.tar.xz
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-${S6_OVERLAY_ARCH}.tar.xz /tmp
RUN tar -C /out -Jxpf /tmp/s6-overlay-${S6_OVERLAY_ARCH}.tar.xz

# add s6 optional symlinks
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-symlinks-noarch.tar.xz /tmp
RUN tar -C /out -Jxpf /tmp/s6-overlay-symlinks-noarch.tar.xz
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-symlinks-arch.tar.xz /tmp
RUN tar -C /out -Jxpf /tmp/s6-overlay-symlinks-arch.tar.xz

# install openresty, /var/run/openresty, /www <- /usr/local/openresty/nginx/html/
COPY --from=openresty /usr/local/openresty /out/usr/local/openresty
COPY --from=openresty /etc/nginx           /out/etc/nginx
RUN  mkdir /out/var/run/openresty

# runtime stage
FROM scratch
COPY --from=rootfs-stage /out/ /
COPY root/ /

LABEL maintainer="suisrc@outlook.com"

# set environment variables
ENV NODE_VERSION="18.19.0" \
    VSCR_VERSION="4.19.1" \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME="0" \
    S6_VERBOSITY=1 \
    LANGUAGE="en_US.UTF-8" \
    LANG="en_US.UTF-8" \
    TERM="xterm" \
    PATH=/usr/local/node/bin:$PATH \
    EXTENSIONS="" \
    VSC_HOME="/vsc" \
    USERNAME="user" \
    HOME="/home/user"

# update linux
RUN apt update && \
    DEBIAN_FRONTEND=noninteractive \
    apt install --no-install-recommends -y \
    dpkg \
    sudo \
    bash \
    zsh \
    vim \
    nano \
    jq \
    curl \
    git \
    procps \
    net-tools \
    iputils-ping \
    netcat \
    ntpdate \
    tzdata \
    unzip \
    p7zip \
    xz-utils \
    locales \
    inotify-tools \
    ca-certificates \
    openssh-server \
    python3 \
    gcc \
    binutils \
    libxfont2 \
    libdbus-glib-1-2 \
    libatomic1 \
    fontconfig \
    build-essential \
    libz-dev \ 
    zlib1g-dev \
    fonts-noto-core \
    fonts-noto-cjk \
    fonts-noto-color-emoji &&\
    fc-cache -f -v && \
    locale-gen en_US.UTF-8 && \
    mkdir -p /wsc && ln -s /wsc /home/wsc && \
    ln -s /usr/local/openresty/nginx/sbin/nginx /usr/bin/nginx && \
    ln -s /usr/local/openresty/nginx/html       /www && \
    apt-get autoremove && apt-get clean && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# creating the user and usergroup
RUN groupadd --gid 1000 $USERNAME && \
    useradd  --uid 1000 --gid $USERNAME -d $HOME -m -s /bin/bash $USERNAME   && \
    echo $USERNAME ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/$USERNAME && \
    chmod 0440 /etc/sudoers.d/$USERNAME && chmod g+rw /home && \
    chown $USERNAME:$USERNAME /etc/nginx/conf.d /usr/local/openresty/nginx/html

WORKDIR $HOME

# # install oh-my-zsh
# RUN curl -fsSL "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh" &&\
#     git clone "https://github.com/zsh-users/zsh-autosuggestions" ~/.oh-my-zsh/plugins/zsh-autosuggestions &&\
#     echo "source ~/.oh-my-zsh/plugins/zsh-autosuggestions/zsh-autosuggestions.zsh" >> ~/.zshrc &&\
#     sed -i "1iZSH_DISABLE_COMPFIX=true" ~/.zshrc &&\
#     apt-get autoremove && apt-get clean && rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# =============================================================================================
# install nodejs and vscode <- https://nodejs.org/en/
RUN mkdir /usr/local/node && \
    curl -fSL --compressed "https://nodejs.org/dist/v${NODE_VERSION}/node-v$NODE_VERSION-linux-x64.tar.xz" | \
    tar -xJ -C /usr/local/node --strip-components=1 && npm install -g yarn && node --version && npm --version && \
    VSC_RURL="https://github.com/coder/code-server/releases" &&\
    VSC_PATH="${VSC_RURL}/download/v${VSCR_VERSION}/code-server-${VSCR_VERSION}-linux-amd64.tar.gz" &&\
    curl -o /tmp/vsc.tar.gz -L "${VSC_PATH}" && mkdir -p ${VSC_HOME} && tar xzf /tmp/vsc.tar.gz -C ${VSC_HOME}/ --strip-components=1 && \
    rm -f ${VSC_HOME}/node      && ln -s /usr/local/node/bin/node ${VSC_HOME}/node && \
    rm -f ${VSC_HOME}/lib/node  && ln -s /usr/local/node/bin/node ${VSC_HOME}/lib/node && \
    rm -f ${VSC_HOME}/lib/coder-cloud-agent && \
    ln -s ${VSC_HOME}/bin/code-server /usr/bin/code-server && \
    ${VSC_HOME}/bin/code-server --install-extension mhutchie.git-graph && \
    ${VSC_HOME}/bin/code-server --install-extension esbenp.prettier-vscode && \
    ${VSC_HOME}/bin/code-server --install-extension humao.rest-client && \
    rm -rf /tmp/* /var/tmp/* $HOME/.local/share/code-server/CachedExtensionVSIXs/* && \
    chown -R $USERNAME:$USERNAME ${VSC_HOME} $HOME /wsc

ENTRYPOINT ["/init"]
EXPOSE 7000

# =============================================================================================

# env
ENV KASM_VERSION="1.2.0" \
    DISPLAY=:1 \
    PERL5LIB=/usr/local/bin \
    OMP_WAIT_POLICY=PASSIVE \
    GOMP_SPINCOUNT=0 \
    PULSE_RUNTIME_PATH=/defaults \
    NVIDIA_DRIVER_CAPABILITIES=all

# update
RUN apt update && \
    DEBIAN_FRONTEND=noninteractive \
    apt install --no-install-recommends -y \
    cups \
    cups-client \
    cups-pdf \
    dbus-x11 \
    ffmpeg \
    file \
    fuse-overlayfs \
    intel-media-va-driver \
    libdatetime-perl \
    libfontenc1 \
    libfreetype6 \
    libgbm1 \
    libgcrypt20 \
    libgl1-mesa-dri \
    libglu1-mesa \
    libgnutls30 \
    libgomp1 \
    libhash-merge-simple-perl \
    libjpeg-turbo8 \
    liblist-moreutils-perl \
    libp11-kit0 \
    libpam0g \
    libpixman-1-0 \
    libscalar-list-utils-perl \
    libswitch-perl \
    libtasn1-6 \
    libtry-tiny-perl \
    libwebp7 \
    libx11-6 \
    libxau6 \
    libxcb1 \
    libxcursor1 \
    libxdmcp6 \
    libxext6 \
    libxfixes3 \
    libxfont2 \
    libxinerama1 \
    libxshmfence1 \
    libxtst6 \
    libyaml-tiny-perl \
    mesa-va-drivers \
    openbox \
    openssh-client \
    openssl \
    pciutils \
    perl \
    procps \
    pulseaudio \
    pulseaudio-utils \
    software-properties-common \
    ssl-cert \
    util-linux \
    x11-apps \
    x11-common \
    x11-utils \
    x11-xkb-utils \
    x11-xkb-utils \
    x11-xserver-utils \
    xauth \
    xdg-utils \
    xfonts-base \
    xkb-data \
    xserver-common \
    xserver-xorg-core \
    xserver-xorg-video-amdgpu \
    xserver-xorg-video-ati \
    xserver-xorg-video-intel \
    xserver-xorg-video-nouveau \
    xserver-xorg-video-qxl \
    xterm \
    xutils \
    zlib1g &&\
    apt-get autoremove && apt-get clean && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# kasm vnc
# COPY --from=kasm-stage /build-out/ /
RUN APP_URL="https://github.com/kasmtech/KasmVNC/releases/download/v${KASM_VERSION}/kasmvncserver_jammy_${KASM_VERSION}_amd64.deb" && \
    curl -o /tmp/app.deb -L "${APP_URL}" && dpkg -i /tmp/app.deb && \
    apt-get autoremove && apt-get clean && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# kclient
COPY --from=kclient-stage /kclient /kclient




