FROM ghcr.io/suisrc/vscode:1.83.1-cdr-ubuntu

RUN  mkdir -p /workspace/.go/bin
USER root

ENV GO_VERSION=1.21.3 \
    JAVA_VERSION=jdk-17.0.8.1+1_openj9-0.40.0 \
    MAVEN_VERSION=3.9.5 \
    GOPATH=/workspace/.go \
    JDK_HOME=/usr/local/java \
    JAVA_HOME=/usr/local/java \
    MAVEN_HOME=/usr/local/maven \
    PATH=/usr/local/golang/bin:/usr/local/node/bin:/usr/local/java/bin:/usr/local/maven/bin:/workspace/.go/bin:$PATH

# ==============================================================================================================
# https://golang.google.cn/dl/
RUN mkdir /usr/local/golang && \
    curl -fSL --compressed "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz" | \
    tar -xz -C /usr/local/golang --strip-components=1 &&\
    go version

# ==============================================================================================================
# https://github.com/AdoptOpenJDK/openjdk11-binaries/releases
# https://github.com/AdoptOpenJDK/semeru[11,17]-binaries/releases/**/ibm-semeru-open-jdk_x64_linux_xxx.tar.gz
# 11 => jdk-11.0.17+8_openj9-0.35.0  17 => jdk-17.0.5+8_openj9-0.35.0
RUN if [ -z ${JAVA_URL+x} ]; then \
        if [ -z ${JAVA_VERSION+x} ]; then \
            JAVA_VERSION=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/semeru17-binaries/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        JAVA_URL=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/semeru17-binaries/releases/tags/${JAVA_VERSION}" \
            | jq -r 'first(.assets[] | select(.browser_download_url | contains("jdk_x64_linux_") and endswith(".tar.gz") ) | .browser_download_url)'); \
    fi &&\
    mkdir /usr/local/java && \
    curl -fSL --compressed ${JAVA_URL} | \
    tar -xz -C /usr/local/java --strip-components=1 &&\
    java -version

# http://mirrors.tuna.tsinghua.edu.cn/apache/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz"
# https://downloads.apache.org/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz
RUN if [ -z ${MAVEN_URL+x} ]; then \
        MAVEN_URL="https://downloads.apache.org/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz"; \
    fi &&\
    mkdir /usr/local/maven &&\
    curl -fSL --compressed ${MAVEN_URL} | \
    tar -xz -C /usr/local/maven --strip-components=1 &&\
    sed -i -e "159d" -e "s/  <\/mirrors>/    -->\n&/g" /usr/local/maven/conf/settings.xml &&\
    mvn -version

# ==============================================================================================================
# RUN apt update && apt install -y python3 python3-pip && ln -s /usr/bin/python3 /usr/local/bin/py &&\
#     apt autoremove -y && apt clean && rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# ==============================================================================================================
USER vscode
# golang extension
RUN go install github.com/ramya-rao-a/go-outline@latest &&\
    go install github.com/cweill/gotests/gotests@latest &&\
    go install github.com/fatih/gomodifytags@latest &&\
    go install github.com/josharian/impl@latest &&\
    go install github.com/haya14busa/goplay/cmd/goplay@latest &&\
    go install github.com/go-delve/delve/cmd/dlv@latest &&\
    go install honnef.co/go/tools/cmd/staticcheck@latest &&\
    go install golang.org/x/tools/gopls@latest &&\
    go install github.com/google/wire/cmd/wire@latest; exit 0

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