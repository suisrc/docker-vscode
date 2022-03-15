# FROM suisrc/vscode:debian
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.65.2-debian

ENV NODE_VERSION v16.14.0

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
    curl -fsSLO --compressed "https://nodejs.org/dist/$NODE_VERSION/node-$NODE_VERSION-linux-$ARCH.tar.xz" &&\
    #curl -fsSLO --compressed "https://nodejs.org/dist/$NODE_VERSION/SHASUMS256.txt.asc" &&\
    #gpg --batch --decrypt --output SHASUMS256.txt SHASUMS256.txt.asc &&\
    #grep " node-$NODE_VERSION-linux-$ARCH.tar.xz\$" SHASUMS256.txt | sha256sum -c - &&\
    #tar -xJf "node-$NODE_VERSION-linux-$ARCH.tar.xz" -C /usr/local --strip-components=1 --no-same-owner &&\
    tar -xzf "node-$NODE_VERSION-linux-$ARCH.tar.xz" -C /usr/local --strip-components=1 --no-same-owner &&\
    #rm "node-$NODE_VERSION-linux-$ARCH.tar.xz" SHASUMS256.txt.asc SHASUMS256.txt &&\
    rm "node-$NODE_VERSION-linux-$ARCH.tar.xz"  &&\
    ln -s /usr/local/bin/node /usr/local/bin/nodejs &&\
    # smoke tests
    node --version &&\
    npm  --version

# config china npm and aliyun yarn
RUN npm install -g cnpm yarn tyarn

USER vscode
# extension
RUN code-server --install-extension jcbuisson.vue
