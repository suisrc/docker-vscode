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
    echo "**** applications tweaks ****" && \
    sed -i "s/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/g" /etc/locale.gen && locale-gen &&\
    fc-cache -fv && \
    ln -s /usr/local/openresty/nginx/sbin/nginx /usr/bin/nginx && \
    ln -s /usr/local/openresty/nginx/html /www && mkdir /var/run/openresty && \
    echo "**** cleanup ****" && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# fcitx5 fcitx5-pinyin??

# Creating the user and usergroup
ARG USERNAME=wsc
RUN groupadd --gid 1000 $USERNAME && \
    useradd  --uid 1000 --gid $USERNAME -m -s /bin/bash $USERNAME && \
    echo $USERNAME ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/$USERNAME && \
    echo "$USERNAME:abc123" | chpasswd && usermod -aG sudo $USERNAME && \
    chmod 0440 /etc/sudoers.d/$USERNAME && chmod g+rw /home

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
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0 \
    PATH="$PATH:/command" \

WORKDIR    /home/$USERNAME
ENTRYPOINT ["/init"]

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
