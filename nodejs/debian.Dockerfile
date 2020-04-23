# https://github.com/suisrc/docker-code-server/blob/master/debian.Dockerfile
# https://github.com/nodejs/docker-node/blob/master/12/stretch/Dockerfile
# 
# https://github.com/suisrc/docker-code-server/releases
# 
# https://hub.docker.com/_/node
# https://hub.docker.com/r/suisrc/vscode
# FROM node:12-stretch
FROM suisrc/vscode:1.43.2-3.1.1-debian9

ENV NODE_VERSION 12.16.2
ENV YARN_VERSION 1.22.4

RUN echo "**** update linux ****" && \
    apt-get update && \
    apt-get install --no-install-recommends -y \
        dpkg &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

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
    #curl -fsSLO --compressed "https://nodejs.org/dist/v$NODE_VERSION/SHASUMS256.txt.asc" &&\
    #gpg --batch --decrypt --output SHASUMS256.txt SHASUMS256.txt.asc &&\
    #grep " node-v$NODE_VERSION-linux-$ARCH.tar.xz\$" SHASUMS256.txt | sha256sum -c - &&\
    #tar -xJf "node-v$NODE_VERSION-linux-$ARCH.tar.xz" -C /usr/local --strip-components=1 --no-same-owner &&\
    tar -xzf "node-v$NODE_VERSION-linux-$ARCH.tar.gz" -C /usr/local --strip-components=1 --no-same-owner &&\
    #rm "node-v$NODE_VERSION-linux-$ARCH.tar.xz" SHASUMS256.txt.asc SHASUMS256.txt &&\
    rm "node-v$NODE_VERSION-linux-$ARCH.tar.gz"  &&\
    ln -s /usr/local/bin/node /usr/local/bin/nodejs &&\
    # smoke tests
    node --version &&\
    npm  --version

# yarn
RUN echo "**** install yarn ****" &&\
    set -ex &&\
    curl -fsSLO --compressed "https://yarnpkg.com/downloads/$YARN_VERSION/yarn-v$YARN_VERSION.tar.gz" &&\
    #curl -fsSLO --compressed "https://yarnpkg.com/downloads/$YARN_VERSION/yarn-v$YARN_VERSION.tar.gz.asc" &&\
    #gpg --batch --verify yarn-v$YARN_VERSION.tar.gz.asc yarn-v$YARN_VERSION.tar.gz &&\
    mkdir -p /opt &&\
    tar -xzf yarn-v$YARN_VERSION.tar.gz -C /opt/ &&\
    ln -s /opt/yarn-v$YARN_VERSION/bin/yarn /usr/local/bin/yarn &&\
    ln -s /opt/yarn-v$YARN_VERSION/bin/yarnpkg /usr/local/bin/yarnpkg &&\
    #rm yarn-v$YARN_VERSION.tar.gz.asc yarn-v$YARN_VERSION.tar.gz &&\
    rm yarn-v$YARN_VERSION.tar.gz &&\
    # smoke test
    yarn --version

# config aliyun npm registry
RUN npm install --production -g cnpm --registry=https://registry.npm.taobao.org &&\
    npm config set registry https://registry.npm.taobao.org --global &&\
    npm config set disturl https://npm.taobao.org/dist --global

