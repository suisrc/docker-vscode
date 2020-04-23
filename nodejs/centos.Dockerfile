FROM suisrc/vscode:1.43.2-3.1.1-centos7

ENV NODE_VERSION 12.16.2
ENV YARN_VERSION 1.22.4

RUN echo "**** update linux ****" && \
    yum clean all && yum makecache && yum update -y &&\
    yum install -y \
        dpkg &&\
    rm -rf /tmp/* /var/tmp/* /var/cache/yum  

# nodejs
RUN echo "**** install nodejs ****" &&\
    ARCH= && dpkgArch="$(dpkg --print-architecture)" \
    && case "${dpkgArch##*-}" in \
      amd64) ARCH='x64';; \
      ppc64el) ARCH='ppc64le';; \
      s390x) ARCH='s390x';; \
      arm64) ARCH='arm64';; \
      armhf) ARCH='armv7l';; \
      i386) ARCH='x86';; \
      *) echo "unsupported architecture"; exit 1 ;; \
    esac &&\
    set -ex &&\
    curl -fsSLO --compressed "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-$ARCH.tar.gz" &&\
    tar -xzf "node-v$NODE_VERSION-linux-$ARCH.tar.gz" -C /usr/local --strip-components=1 --no-same-owner &&\
    rm "node-v$NODE_VERSION-linux-$ARCH.tar.gz"  &&\
    ln -s /usr/local/bin/node /usr/local/bin/nodejs &&\
    # smoke tests
    node --version &&\
    npm  --version

# yarn
RUN echo "**** install yarn ****" &&\
    set -ex &&\
    curl -fsSLO --compressed "https://yarnpkg.com/downloads/$YARN_VERSION/yarn-v$YARN_VERSION.tar.gz" &&\
    mkdir -p /opt &&\
    tar -xzf yarn-v$YARN_VERSION.tar.gz -C /opt/ &&\
    ln -s /opt/yarn-v$YARN_VERSION/bin/yarn /usr/local/bin/yarn &&\
    ln -s /opt/yarn-v$YARN_VERSION/bin/yarnpkg /usr/local/bin/yarnpkg &&\
    rm yarn-v$YARN_VERSION.tar.gz &&\
    # smoke test
    yarn --version

# config aliyun npm registry
RUN npm install --production -g cnpm --registry=https://registry.npm.taobao.org &&\
    npm config set registry https://registry.npm.taobao.org --global &&\
    npm config set disturl https://npm.taobao.org/dist --global

