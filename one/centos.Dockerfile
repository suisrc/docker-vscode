#FROM suisrc/vscode:centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:centos-graal

ARG GO_VER=1.14.2
ARG GO_URL=https://dl.google.com/go/go${GO_VER}.linux-amd64.tar.gz

ARG PY_VER=3.8.3
ARG PY_URL=https://www.python.org/ftp/python/${PY_VER}/Python-${PY_VER}.tgz

ARG GRAALVM_RELEASE=vm-20.1.0
ARG JAVA_RELEASE=java11
ARG GRAALVM_URL

ARG MAVEN_RELEASE=3.6.3
ARG MAVEN_URL=https://downloads.apache.org/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz

# install python
RUN curl -fSL --compressed  $PY_URL -o python-autoconf.tar.gz &&\
    mddir python-autoconf && mkdir -p /usr/local/python3 &&\
    tar -zxvf python-autoconf.tar.gz -C python-autoconf --strip-components=1 &&\
    cd python-autoconf &&\
    ./configure --prefix=/usr/local/python3 --with-ssl &&\
    make && make install &&\
    cd .. && rm -rf python-autoconf &&\
    ln -s /usr/local/python3/bin/python3 /usr/local/bin/python3 &&\
    ln -s /usr/local/python3/bin/pip3    /usr/local/bin/pip3 &&\
    ln -s /usr/local/python3/bin/python3 /usr/local/bin/py &&\
    pip3 install --upgrade pip && pip3 --version

# python extension
RUN pip3 install --user pylint &&\
    pip3 install --user django &&\
    pip3 list
    # ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin

# ENV for user, no config
# ENV PATH=/root/.local/bin:$PATH

# install golang
RUN curl -fSL --compressed $GO_URL | tar -xz -C /usr/local

ENV PATH=/usr/local/go/bin:$PATH
ENV GOPATH=/root/go
ENV GOPROXY=

# golang env
RUN go env -w GO111MODULE=on &&\
    go env -w GOPROXY=https://goproxy.io,direct

# golang extension
RUN go get -u github.com/mdempsky/gocode &&\
    go get -u github.com/uudashr/gopkgs/v2/cmd/gopkgs &&\
    go get -u github.com/ramya-rao-a/go-outline &&\
    go get -u github.com/acroca/go-symbols &&\
    go get -u github.com/cweill/gotests &&\
    go get -u github.com/fatih/gomodifytags &&\
    go get -u github.com/josharian/impl &&\
    go get -u github.com/davidrjenni/reftools/cmd/fillstruct &&\
    go get -u github.com/haya14busa/goplay/cmd/goplay &&\
    go get -u github.com/godoctor/godoctor &&\
    go get -u github.com/go-delve/delve/cmd/dlv &&\
    go get -u github.com/stamblerre/gocode &&\
    go get -u github.com/rogpeppe/godef &&\
    go get -u github.com/sqs/goreturns &&\
    go get -u golang.org/x/lint/golint &&\
    go get -u golang.org/x/tools/cmd/goimports &&\
    go get -u golang.org/x/tools/gopls &&\
    go get -u golang.org/x/tools/cmd/guru &&\
    go get -u golang.org/x/tools/cmd/gorename; exit 0

# install oracle graalvm-ce 
RUN echo "**** install graalvm-ce ****" &&\
    set -eux &&\
    if [ -z ${GRAALVM_URL+x} ]; then \
        if [ -z ${GRAALVM_RELEASE+x} ]; then \
            GRAALVM_RELEASE=$(curl -sX GET "https://api.github.com/repos/graalvm/graalvm-ce-builds/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        GRAALVM_URL="https://github.com/graalvm/graalvm-ce-builds/releases/download/${GRAALVM_RELEASE}/graalvm-ce-${JAVA_RELEASE}-linux-amd64-${GRAALVM_RELEASE##*-}.tar.gz"; \
        #GRAALVM_URL=$(curl -sX GET "https://api.github.com/repos/graalvm/graalvm-ce-builds/releases/tags/${GRAALVM_RELEASE}" \
        #    | jq -r '.assets[] | select(.browser_download_url | contains("graalvm-ce-java11-linux-amd64")) | .browser_download_url'); \
        # https://github.com/graalvm/graalvm-ce-builds/releases/download/vm-20.0.0/graalvm-ce-java11-linux-amd64-20.0.0.tar.gz
    fi &&\
    mkdir -p /graalvm &&\
    #curl `#--fail --silent --location --retry 3` -fSL ${GRAALVM_URL} | tar -zxC /graalvm --strip-components=1 &&\
    curl -fsL --compressed ${GRAALVM_URL} -o graalvm-ce.tar.gz &&\
    tar -xzf graalvm-ce.tar.gz -C /graalvm --strip-components=1 &&\
    rm -f graalvm-ce.tar.gz

ENV PATH=/graalvm/bin:$PATH
RUN gu install native-image

ENV JDK_HOME=/graalvm
ENV JAVA_HOME=/graalvm

# install mvn
RUN mkdir -p /usr/share/maven &&\
    curl -fSL ${MAVEN_URL} | tar -xzC /usr/share/maven --strip-components=1 &&\
    ln -s /usr/share/maven/bin/mvn /usr/bin/mvn &&\
    mvn -version

ENV MAVEN_HOME /usr/share/maven

# nodejs extension
RUN npm install -g cnpm yarn tyarn &&\
    npm config set registry https://registry.npm.taobao.org --global &&\
    npm config set disturl https://npm.taobao.org/dist --global

# vscode extension
RUN code-server --install-extension ms-vscode.go &&\
    code-server --install-extension ms-python.python &&\
    code-server --install-extension redhat.vscode-yaml &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension vscjava.vscode-java-pack &&\
    code-server --install-extension eamodio.gitlens

# 增加开发环境测试用例
COPY test.* /home/test/
COPY README.md /home/test/