FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.64.2-cdr-debian

USER root

ENV GO_VERSION=1.18 \
    NODE_VERSION=16.14.2 \
    JAVA_VERSION=jdk-11.0.11+9_openj9-0.26.0 \
    MAVEN_VERSION=3.8.5

# ==============================================================================================================
# https://golang.google.cn/dl/
RUN curl -fSL --compressed "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz" | tar -xz -C /usr/local && go version
ENV PATH=/usr/local/go/bin:/workspace/.go/bin:$PATH \
    GOPATH=/workspace/.go

# ==============================================================================================================
# https://nodejs.org/en/
RUN curl -fSL --compressed "https://nodejs.org/dist/v${NODE_VERSION}/node-v$NODE_VERSION-linux-x64.tar.xz" | tar -x -C /usr/local --strip-components=1 --no-same-owner &&\
    npm install -g cnpm yarn tyarn && node --version && npm --version

# ==============================================================================================================
# https://github.com/AdoptOpenJDK/openjdk11-binaries/releases
RUN if [ -z ${JAVA_URL+x} ]; then \
        if [ -z ${JAVA_VERSION+x} ]; then \
            JAVA_VERSION=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk11-binaries/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        JAVA_URL=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk11-binaries/releases/tags/${JAVA_VERSION}" \
            | jq -r 'first(.assets[] | select(.browser_download_url | contains("jdk_x64_linux_openj9_") and endswith(".tar.gz") ) | .browser_download_url)'); \
    fi &&\
    mkdir -p /usr/lib/jvm/java-adopt && curl -fSL --compressed ${JAVA_URL} | tar -xz -C /usr/lib/jvm/java-adopt --strip-components=1 &&\
    /usr/lib/jvm/java-adopt/bin/java -version

ENV PATH=/usr/lib/jvm/java-adopt/bin:$PATH \
    JDK_HOME=/usr/lib/jvm/java-adopt  \
    JAVA_HOME=/usr/lib/jvm/java-adopt \
    MAVEN_HOME=/usr/share/maven

# http://mirrors.tuna.tsinghua.edu.cn/apache/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz"
# https://downloads.apache.org/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz
RUN if [ -z ${MAVEN_URL+x} ]; then \
        MAVEN_URL="https://downloads.apache.org/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz"; \
    fi &&\
    mkdir -p /usr/share/maven && curl -fSL --compressed ${MAVEN_URL} | tar -xz -C /usr/share/maven --strip-components=1 &&\
    sed -i -e "158d" -e "s/  <\/mirrors>/    -->\n&/g" /usr/share/maven/conf/settings.xml &&\
    ln -s /usr/share/maven/bin/mvn /usr/local/bin/mvn && mvn -version

# ==============================================================================================================
# RUN apt update && apt install -y python3 python3-pip && ln -s /usr/bin/python3 /usr/local/bin/py &&\
#     apt autoremove -y && apt clean && rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# ==============================================================================================================
USER vscode
# golang extension
RUN mkdir /workspace/.go &&\
    go install github.com/ramya-rao-a/go-outline@latest &&\
    go install github.com/cweill/gotests/gotests@latest &&\
    go install github.com/fatih/gomodifytags@latest &&\
    go install github.com/josharian/impl@latest &&\
    go install github.com/haya14busa/goplay/cmd/goplay@latest &&\
    go install github.com/go-delve/delve/cmd/dlv@latest &&\
    go install honnef.co/go/tools/cmd/staticcheck@latest &&\
    go install golang.org/x/tools/gopls@latest; exit 0

# python extension
# RUN pip3 install --upgrade pip &&\
#     pip3 install --user pylint &&\
#     pip3 install --user django

# vscode extension
# RUN code-server --install-extension golang.go &&\
#     code-server --install-extension ms-python.python &&\
#     code-server --install-extension vscjava.vscode-java-pack &&\
#     code-server --install-extension gabrielbb.vscode-lombok &&\
#     rm -rf $USERDATA/CachedExtensionVSIXs/*