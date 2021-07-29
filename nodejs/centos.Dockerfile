#FROM suisrc/vscode:centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.57.1-centos

ENV NODE_VERSION v14.17.3

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
    curl -fsSLO --compressed "https://nodejs.org/dist/$NODE_VERSION/node-$NODE_VERSION-linux-$ARCH.tar.gz" &&\
    tar -xzf "node-$NODE_VERSION-linux-$ARCH.tar.gz" -C /usr/local --strip-components=1 --no-same-owner &&\
    rm "node-$NODE_VERSION-linux-$ARCH.tar.gz"  &&\
    ln -s /usr/local/bin/node /usr/local/bin/nodejs &&\
    # smoke tests
    node --version &&\
    npm  --version

# config china npm and aliyun yarn
RUN npm install -g cnpm yarn tyarn

