# syntax=docker/dockerfile:1
# empty image=scratch
FROM suisrc/openresty:1.21.4.1-hu-3 as openresty

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
ARG VSCR_BIN="/vsc"
ARG USERNAME="user"

ENV NODE_VERSION="18.19.0" \
    VSCR_VERSION="4.19.1" \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME="0" \
    S6_VERBOSITY=1 \
    LANGUAGE="en_US.UTF-8" \
    LANG="en_US.UTF-8" \
    TERM="xterm" \
    HOME="/home/$USERNAME" \
    PATH=/usr/local/node/bin:$PATH \
    EXTENSIONS=""

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
    ssl-cert \
    ca-certificates \
    openssh-server \
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
    mkdir -p /home/$USERNAME/project && \
    ln -s /home/$USERNAME/project /ws && \
    ln -s /usr/local/openresty/nginx/sbin/nginx /usr/bin/nginx && \
    ln -s /usr/local/openresty/nginx/html       /www && \
    apt-get autoremove && apt-get clean && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# creating the user and usergroup
RUN groupadd --gid 1000 $USERNAME && \
    useradd  --uid 1000 --gid $USERNAME -d $HOME -m -s /bin/bash $USERNAME   && \
    echo $USERNAME ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/$USERNAME && \
    chmod 0440 /etc/sudoers.d/$USERNAME && chmod g+rw /home

WORKDIR $HOME
# https://github.com/just-containers/s6-overlay
ENTRYPOINT ["/init"]

# install oh-my-zsh
RUN curl -fsSL "https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh" &&\
    git clone "https://github.com/zsh-users/zsh-autosuggestions" ~/.oh-my-zsh/plugins/zsh-autosuggestions &&\
    echo "source ~/.oh-my-zsh/plugins/zsh-autosuggestions/zsh-autosuggestions.zsh" >> ~/.zshrc &&\
    sed -i "1iZSH_DISABLE_COMPFIX=true" ~/.zshrc && rm -rf ~/.oh-my-zsh/plugins/zsh-autosuggestions/.git &&\
    apt-get autoremove && apt-get clean && rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/* &&\
    chown -R $USERNAME:$USERNAME $HOME && chown -R $USERNAME:$USERNAME /usr

# =============================================================================================
# https://nodejs.org/en/
RUN mkdir /usr/local/node && \
    curl -fSL --compressed "https://nodejs.org/dist/v${NODE_VERSION}/node-v$NODE_VERSION-linux-x64.tar.xz" | \
    tar -xJ -C /usr/local/node --strip-components=1 && npm install -g yarn && node --version && npm --version
# vscode-server
RUN VSC_RURL="https://github.com/coder/code-server/releases" &&\
    VSC_PATH="${VSC_RURL}/download/v${VSCR_VERSION}/code-server-${VSCR_VERSION}-linux-amd64.tar.gz" &&\
    curl -o /tmp/vsc.tar.gz -L "${VSC_PATH}" && mkdir -p ${VSCR_BIN} && tar xzf /tmp/vsc.tar.gz -C ${VSCR_BIN}/ --strip-components=1 && \
    ln -s ${VSCR_BIN}/bin/code-server /usr/bin/code-server && \
    rm -f ${VSCR_BIN}/node      && ln -s /usr/local/node/bin/node ${VSCR_BIN}/node &&\
    rm -f ${VSCR_BIN}/lib/node  && ln -s /usr/local/node/bin/node ${VSCR_BIN}/lib/node &&\
    rm -f ${VSCR_BIN}/lib/coder-cloud-agent &&\
    chown -R $USERNAME:$USERNAME ${VSCR_BIN} && \
    rm -rf /tmp/* /var/tmp/*

# =============================================================================================
# default user
USER $USERNAME
# install extension ?ms-ceintl.vscode-language-pack-zh-hans
RUN code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode &&\
    code-server --install-extension humao.rest-client &&\
    rm -rf $HOME/.local/share/code-server/CachedExtensionVSIXs/*

EXPOSE 7000

