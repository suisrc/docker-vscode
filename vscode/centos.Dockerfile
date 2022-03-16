FROM centos:7

LABEL maintainer="suisrc@outlook.com"

ARG VSC_RURL=https://github.com/gitpod-io/openvscode-server/releases
ARG VSC_RELEASE=v1.65.2
ARG VSC_URL=${VSC_RURL}/download/openvscode-server-${VSC_RELEASE}/openvscode-server-${VSC_RELEASE}-linux-x64.tar.gz
ARG VSC_HOME=/vsc

ARG S6_RELEASE=v3.1.0.1
ARG S6_APP=https://github.com/just-containers/s6-overlay/releases/download/${S6_RELEASE}/s6-overlay-x86_64.tar.xz
ARG S6_CFG=https://github.com/just-containers/s6-overlay/releases/download/${S6_RELEASE}/s6-overlay-noarch.tar.xz

# https://github.com/git/git/releases
ARG GIT_RELEASE=v2.33.1
ARG GIT_URL=https://github.com/git/git/archive/${GIT_RELEASE}.tar.gz

# https://www.sqlite.org/download.html
ARG SQLITE_URL=https://www.sqlite.org/2022/sqlite-autoconf-3380100.tar.gz

#ARG LINUX_MIRRORS=http://mirrors.aliyun.com
# update linux
RUN yum clean all && yum install -y epel-release && yum makecache && yum update -y && yum install -y \
    sudo curl jq net-tools zsh vim nano ntpdate openssh-server xz openssl make p7zip fontconfig gcc dpkg libstdc++-static gcc-c++ \
    glibc-devel zlib-devel openssl-devel libffi-devel curl-devel expat-devel gettext-devel && \
    rm -rf /tmp/* /var/tmp/* /var/cache/yum
# localedef -c -f UTF-8 -i zh_CN.UTF8

# git版本低， 无法和vscode兼容
RUN curl -fSL $GIT_URL -o /tmp/git-autoconf.tar.gz &&\
    mkdir /tmp/git-autoconf && tar -zxf /tmp/git-autoconf.tar.gz -C /tmp/git-autoconf --strip-components=1 &&\
    cd /tmp/git-autoconf && make prefix=/usr/local && make prefix=/usr/local install &&\
    mv /usr/bin/git  /usr/bin/git_old &&\
    ln -s /usr/local/bin/git  /usr/bin/git &&\
    git version && rm -rf /tmp/*

# sqlite版本低, 无法和django兼容(python框架，为后面扩展)
RUN curl -fSL $SQLITE_URL -o /tmp/sqlite-autoconf.tar.gz &&\
    mkdir /tmp/sqlite-autoconf && tar -zxf /tmp/sqlite-autoconf.tar.gz -C /tmp/sqlite-autoconf --strip-components=1 &&\
    cd /tmp/sqlite-autoconf && ./configure --prefix=/usr/local && make && make install &&\
    mv /usr/bin/sqlite3  /usr/bin/sqlite3_old &&\
    ln -s /usr/local/bin/sqlite3   /usr/bin/sqlite3 &&\
    echo "/usr/local/lib" > /etc/ld.so.conf.d/sqlite3.conf && ldconfig &&\
    sqlite3 -version && rm -rf /tmp/*

# install sarasa-gothic
# ARG FONT_URL
# ARG FONT_RELEASE
ARG FONT_RURL=https://api.github.com/repos/suisrc/Sarasa-Gothic/releases
RUN if [ -z ${FONT_URL+x} ]; then \
        if [ -z ${FONT_RELEASE+x} ]; then \
            FONT_RELEASE=$(curl -sX GET "${FONT_RURL}/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        FONT_URL=$(curl -sX GET "${FONT_RURL}/tags/${FONT_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("sc.7z")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/sarasa-gothic-ttf.7z -L "${FONT_URL}" && \
    mkdir -p /usr/share/fonts/truetype/sarasa-gothic &&\
    cd /usr/share/fonts/truetype/sarasa-gothic &&\
    7za x /tmp/sarasa-gothic-ttf.7z &&\
    fc-cache -f -v &&\
    rm -rf /tmp/*

# =============================================================================================
# s6-overlay
RUN curl -o /tmp/s6-cfg.tar.xz -L "${S6_CFG}" && tar -C / -Jxpf /tmp/s6-cfg.tar.xz &&\
    curl -o /tmp/s6-app.tar.xz -L "${S6_APP}" && tar -C / -Jxpf /tmp/s6-app.tar.xz &&\
    rm -rf  /tmp/*
    #tar xzf /tmp/s6.tar.gz -C / --exclude='./bin' && tar xzf /tmp/s6.tar.gz -C /usr ./bin

COPY init-* /command/
# config s6
COPY s6-init /etc/cont-init.d/vsc
COPY s6-vsc  /etc/services.d/vsc/run
# copy demo
COPY test.*   /home/test/demo/
COPY mirror-* /home/test/mirror/

ARG USERDATA=/workspace/.openvscode-server/data
RUN mkdir /workspace && ln -s /workspace /ws && mkdir -p ${VSC_HOME}
COPY settings1.json /workspace/.vscode/settings.json

# https://github.com/just-containers/s6-overlay
WORKDIR   /workspace
ENTRYPOINT ["/init"]

ENV HOME=/workspace \
    S6_KEEP_ENV=true

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

# Creating the user and usergroup
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID

RUN groupadd --gid $USER_GID $USERNAME && \
    useradd --uid $USER_UID --gid $USERNAME -m -s /bin/bash $USERNAME   && \
    echo $USERNAME ALL=\(root\) NOPASSWD:ALL > /etc/sudoers.d/$USERNAME && \
    chmod 0440 /etc/sudoers.d/$USERNAME && chmod g+rw /home

ENV NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-bundle.crt
# =============================================================================================
# vscode-server
RUN if [ -z ${VSC_URL+x} ]; then \
        if [ -z ${VSC_RELEASE+x} ]; then \
            VSC_RELEASE=$(curl -sX GET "${VSC_RURL}/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        VSC_URL=$(curl -sX GET "${VSC_RURL}/tags/${VSC_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("-linux-x64.tar.gz")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/vsc.tar.gz -L "${VSC_URL}" && mkdir -p ${VSC_HOME} && tar xzf /tmp/vsc.tar.gz -C ${VSC_HOME}/ --strip-components=1 && \
    ln -s ${VSC_HOME}/bin/openvscode-server /usr/bin/code-server &&\
    cp ${VSC_HOME}/bin/remote-cli/openvscode-server ${VSC_HOME}/bin/remote-cli/code &&\
    sed -i 's/"$0"/"$(readlink -f $0)"/' ${VSC_HOME}/bin/remote-cli/code &&\
    ln -s ${VSC_HOME}/bin/remote-cli/code /usr/bin/code &&\
    rm -rf /tmp/*


ENV EDITOR=code \
    VISUAL=code \
    GIT_EDITOR="code --wait"

# install extension ?ms-ceintl.vscode-language-pack-zh-hans
RUN code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode &&\
    code-server --install-extension humao.rest-client

# config for user or machine
COPY locale.json    $USERDATA/Machine/locale.json
COPY settings.json $USERDATA/Machine/settings.json

# =============================================================================================
RUN chown -R $USERNAME:$USERNAME /workspace &&\
    chown -R $USERNAME:$USERNAME ${VSC_HOME}

USER $USERNAME
#EXPOSE 7000