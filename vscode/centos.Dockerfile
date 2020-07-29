#FROM centos:7
FROM centos:8

# args

ARG CODE_URL=https://github.com/suisrc/code-server/releases/download/v1.47.3/code-server-3.4.1-linux-amd64.tar.gz
ARG CODE_RELEASE

ARG S6_URL=https://github.com/just-containers/s6-overlay/releases/download/v2.0.0.1/s6-overlay-amd64.tar.gz

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
# COPY RPM-GPG-KEY-* /etc/pki/rpm-gpg/
RUN if [ ! -z ${LINUX_MIRRORS+x} ]; then \
        mv /etc/yum.repos.d/CentOS-Base.repo /etc/yum.repos.d/CentOS-Base.repo.bak &&\
        curl -fsSL ${LINUX_MIRRORS}/repo/Centos-8.repo -o /etc/yum.repos.d/CentOS-Base.repo &&\
        sed -i -e '/mirrors.cloud.aliyuncs.com/d' -e '/mirrors.aliyuncs.com/d' /etc/yum.repos.d/CentOS-Base.repo &&\
        sed -i 's/gpgcheck=1/gpgcheck=0/g' /etc/yum.repos.d/CentOS-Base.repo; \
        #curl -fsSL ${LINUX_MIRRORS}/repo/epel-8.repo -o /etc/yum.repos.d/epel.repo &&\
    fi &&\
    #yum install https://dl.fedoraproject.org/pub/epel/epel-release-latest-8.noarch.rpm
    yum install -y epel-release &&\
    dnf install -y 'dnf-command(config-manager)' && dnf config-manager --set-enabled PowerTools &&\
    yum clean all && yum makecache && yum update -y &&\
    yum install -y sudo curl git jq net-tools zsh p7zip nano fontconfig dpkg openssl  \
                gcc glibc-devel zlib-devel libstdc++-static gcc-c++ make openssl-devel libffi-devel && \
    rm -rf /tmp/* /var/tmp/* /var/cache/yum

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
    7za x /tmp/sarasa-gothic-ttf.7z &&\
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
RUN curl -o /tmp/s6.tar.gz -L "${S6_URL}" && \ 
    tar xzf /tmp/s6.tar.gz -C / --exclude='./bin' && tar xzf /tmp/s6.tar.gz -C /usr ./bin &&\
    # /bin = /usr/bin
    #ln -s /usr/bin/importas /bin/importas &&\
    #ln -s /usr/bin/execlineb /bin/execlineb &&\
    rm -rf /tmp/*

# install code-server
RUN if [ -z ${CODE_URL+x} ]; then \
        if [ -z ${CODE_RELEASE+x} ]; then \
            CODE_RELEASE=$(curl -sX GET "https://api.github.com/repos/cdr/code-server/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        CODE_URL=$(curl -sX GET "https://api.github.com/repos/cdr/code-server/releases/tags/${CODE_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("linux-amd64.tar.gz")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/code.tar.gz -fSL $CODE_URL && \
    mkdir -p /usr/lib/code-server &&\
    tar xzf /tmp/code.tar.gz -C /usr/lib/code-server/ --strip-components=1 && \
    ln -s /usr/lib/code-server/bin/code-server /usr/bin/code-server &&\
    rm -rf /tmp/*

# install code server extension
ENV SERVICE_URL=https://marketplace.visualstudio.com/_apis/public/gallery \
    ITEM_URL=https://marketplace.visualstudio.com/items \
    NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-bundle.crt

# install code-server extension
RUN code-server --install-extension ms-ceintl.vscode-language-pack-zh-hans &&\
    code-server --install-extension mhutchie.git-graph &&\
    code-server --install-extension esbenp.prettier-vscode 

# config for user
COPY ["settings.json", "locale.json", "/root/.local/share/code-server/User/"]

# locale & language
# localectl set-locale LANG=zh_CN.UTF-8
# localectl set-locale LANG=zh_CN.UTF-8
#RUN yum install kde-l10n-Chinese -y &&\
#    sed -i "s/n_US.UTF-8/zh_CN.UTF-8/g" /etc/locale.conf
#ENV LANG="zh_CN.UTF-8" \
#    SHELL=/bin/zsh

# worksapce
# 测试过程中发现，如果使用root账户，会导致程序部分插件没有访问User/文件夹的权限
RUN mkdir -p /home/project && mkdir -p /home/test/mirror && mkdir -p /sh/ &&\
    mkdir -p /root/.local/share/code-server/User/globalStorage
# test
COPY init-*.sh /sh/
COPY test.*   /home/test/
COPY mirror-* /home/test/mirror/

WORKDIR /home/project
#VOLUME [ "/home/project" ]

# code-server start
EXPOSE 7000
ENTRYPOINT ["/init"]

RUN mkdir -p /etc/services.d/vscode && \
    echo -e "#!/usr/bin/execlineb -P\ncode-server --bind-addr 0.0.0.0:7000 --disable-telemetry /home/project" > /etc/services.d/vscode/run && \
    chmod +x /etc/services.d/vscode/run &&\
    #echo -e "#!/usr/bin/execlineb -S1\ns6-svscanctl -t /var/run/s6/services" > /etc/services.d/vscode/finish && \
    #chmod +x /etc/services.d/vscode/finish &&\
    echo -e "#!/usr/bin/execlineb -P\n/sh/init-git.sh" > /etc/cont-init.d/git-init &&\
    chmod +x /etc/cont-init.d/git-init
ENV S6_KEEP_ENV=true
