FROM centos:7
# args
ARG CODE_URL
ARG CODE_RELEASE

ARG FONT_URL
ARG FONT_RELEASE

ARG OH_MY_ZSH_SH_URL
ARG OH_MY_ZSH_SUGGES

ARG LINUX_MIRRORS=http://mirrors.aliyun.com

# set version label
LABEL maintainer="suisrc@outlook.com"

ENV container docker
# linux and softs
RUN echo "**** update linux ****" && \
    if [ ! -z ${LINUX_MIRRORS+x} ]; then \
        mv /etc/yum.repos.d/CentOS-Base.repo /etc/yum.repos.d/CentOS-Base.repo.bak &&\
        curl -fsSL ${LINUX_MIRRORS}/repo/Centos-7.repo -o /etc/yum.repos.d/CentOS-Base.repo &&\
        sed -i -e '/mirrors.cloud.aliyuncs.com/d' -e '/mirrors.aliyuncs.com/d' /etc/yum.repos.d/CentOS-Base.repo &&\
        sed -i 's/gpgcheck=1/gpgcheck=0/g' /etc/yum.repos.d/CentOS-Base.repo &&\
        curl -fsSL ${LINUX_MIRRORS}/repo/epel-7.repo -o /etc/yum.repos.d/epel.repo &&\
        echo "[kubernetes]" >> /etc/yum.repos.d/kubernetes.repo &&\
        echo "name=Kubernetes" >> /etc/yum.repos.d/kubernetes.repo &&\
        echo "baseurl=${LINUX_MIRRORS}/kubernetes/yum/repos/kubernetes-el7-x86_64/" >> /etc/yum.repos.d/kubernetes.repo &&\
        echo "enabled=1" >> /etc/yum.repos.d/kubernetes.repo &&\
        echo "gpgcheck=0" >> /etc/yum.repos.d/kubernetes.repo &&\
        echo "repo_gpgcheck=0" >> /etc/yum.repos.d/kubernetes.repo &&\
        echo "gpgkey=${LINUX_MIRRORS}/kubernetes/yum/doc/yum-key.gpg ${LINUX_MIRRORS}/kubernetes/yum/doc/rpm-package-key.gpg" >> /etc/yum.repos.d/kubernetes.repo &&\
        echo "" >> /etc/yum.repos.d/kubernetes.repo; \
    fi &&\
    yum clean all && yum makecache && yum update -y &&\
    yum install -y sudo curl git jq net-tools zsh p7zip nano fontconfig ntpdate dpkg \
                gcc glibc-devel zlib-devel libstdc++-static make && \
    rm -rf /tmp/* /var/tmp/* /var/cache/yum

# sqlite版本低, 无法使用django(python框架，为后面扩展)
RUN curl -L https://www.sqlite.org/2020/sqlite-autoconf-3310100.tar.gz -o sqlite-autoconf.tar.gz &&\
    mkdir sqlite-autoconf &&\
    tar -zxvf sqlite-autoconf.tar.gz -C sqlite-autoconf --strip-components=1 &&\
    cd sqlite-autoconf && ./configure --prefix=/usr/local && make && make install &&\
    cd .. && rm -rf sqlite-autoconf &&\
    mv /usr/bin/sqlite3  /usr/bin/sqlite3_old &&\
    ln -s /usr/local/bin/sqlite3   /usr/bin/sqlite3 &&\
    echo "/usr/local/lib" > /etc/ld.so.conf.d/sqlite3.conf &&\
    ldconfig &&\
    sqlite3 -version

# fonts
RUN echo "**** install sarasa-gothic ****" && \
    if [ -z ${FONT_URL+x} ]; then \
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

# zsh
# https://raw.githubusercontent.com/robbyrussell/oh-my-zsh/master/tools/install.sh => https://gitee.com/mirrors/oh-my-zsh/raw/master/tools/install.sh
# https://github.com/zsh-users/zsh-autosuggestions => https://gitee.com/ncr/zsh-autosuggestions
RUN echo "**** install oh-my-zsh ****" && \
    if [ -z ${OH_MY_ZSH_SH_URL+x} ]; then \
        OH_MY_ZSH_SH_URL="https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh"; \
    fi &&\
    if [ -z ${OH_MY_ZSH_SUGGES+x} ]; then \
        OH_MY_ZSH_SUGGES="https://github.com/zsh-users/zsh-autosuggestions"; \
    fi &&\
    sh -c "$(curl -fsSL ${OH_MY_ZSH_SH_URL})" &&\
    git clone "${OH_MY_ZSH_SUGGES}" /root/.oh-my-zsh/plugins/zsh-autosuggestions &&\
    echo "source ~/.oh-my-zsh/plugins/zsh-autosuggestions/zsh-autosuggestions.zsh" >> /root/.zshrc &&\
    sed -i "s/ZSH_THEME=\"robbyrussell\"/ZSH_THEME=\"agnoster\"/g" /root/.zshrc

# Code-Server
RUN echo "**** install code-server ****" && \
    if [ -z ${CODE_URL+x} ]; then \
        if [ -z ${CODE_RELEASE+x} ]; then \
            CODE_RELEASE=$(curl -sX GET "https://api.github.com/repos/cdr/code-server/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        CODE_URL=$(curl -sX GET "https://api.github.com/repos/cdr/code-server/releases/tags/${CODE_RELEASE}" \
            | jq -r '.assets[] | select(.browser_download_url | contains("linux-x86_64")) | .browser_download_url'); \
    fi &&\
    curl -o /tmp/code.tar.gz -L "${CODE_URL}" && \
    mkdir -p /usr/lib/code-server &&\
    tar xzf /tmp/code.tar.gz -C /usr/lib/code-server/ --strip-components=1 && \
    ln -s /usr/lib/code-server/code-server /usr/bin/code-server &&\
    rm -rf /tmp/*

# install code server extension
ENV SERVICE_URL=https://marketplace.visualstudio.com/_apis/public/gallery \
    ITEM_URL=https://marketplace.visualstudio.com/items \
    NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-bundle.crt

RUN echo "**** install code-server extension ****" && \
    code-server --install-extension ms-ceintl.vscode-language-pack-zh-hans &&\
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

COPY entrypoint.sh /usr/local/bin/

# worksapce
# 测试过程中发现，如果使用root账户，会导致程序部分插件没有访问User/文件夹的权限
RUN mkdir -p /home/project && \
    chmod +x /usr/local/bin/entrypoint.sh &&\
    mkdir -p /root/.local/share/code-server/User/globalStorage

WORKDIR  /home/project
#VOLUME [ "/home/project" ]

# code-server start
EXPOSE 7000
ENTRYPOINT ["entrypoint.sh"]
CMD [ "code-server", "--bind-addr", "0.0.0.0:7000", "--disable-telemetry", "--disable-updates", "/home/project"]


