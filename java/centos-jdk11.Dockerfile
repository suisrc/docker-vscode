#FROM suisrc/vscode:centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:centos

# args
ARG JAVA_RELEASE=jdk-11.0.7+10
ARG JAVA_URL

ARG MAVEN_RELEASE=3.6.3
ARG MAVEN_URL

# jdk
RUN echo "**** install AdoptOpenJDK ****" &&\
    if [ -z ${JAVA_URL+x} ]; then \
        if [ -z ${JAVA_RELEASE+x} ]; then \
            JAVA_RELEASE=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk11-binaries/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        JAVA_URL=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk11-binaries/releases/tags/${JAVA_RELEASE}" \
            | jq -r 'first(.assets[] | select(.browser_download_url | contains("jdk_x64_linux_") and endswith(".tar.gz") ) | .browser_download_url)'); \
    fi &&\
    mkdir -p /usr/lib/jvm/java-adopt &&\
    curl -L ${JAVA_URL} -o /tmp/adopt-open-jdk.tar.gz &&\
    tar -xzf /tmp/adopt-open-jdk.tar.gz -C /usr/lib/jvm/java-adopt --strip-components=1 &&\
    ln -s /usr/lib/jvm/java-adopt/bin/java /usr/bin/java &&\
    rm -rf /tmp/* &&\
    # smoke tests
    java -version

ENV JDK_HOME=/usr/lib/jvm/java-adopt
ENV JAVA_HOME=/usr/lib/jvm/java-adopt

# mvn
# http://mirrors.tuna.tsinghua.edu.cn/apache/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz"
# https://downloads.apache.org/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz
RUN echo "**** install maven ****" &&\
    if [ -z ${MAVEN_URL+x} ]; then \
        MAVEN_URL="https://downloads.apache.org/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz"; \
    fi &&\
    mkdir -p /usr/share/maven &&\
    curl -L ${MAVEN_URL} -o /tmp/apache-maven.tar.gz &&\
    tar -xzf /tmp/apache-maven.tar.gz -C /usr/share/maven --strip-components=1 &&\
    ln -s /usr/share/maven/bin/mvn /usr/bin/mvn &&\
    rm -rf /tmp/* &&\
    # smoke tests
    mvn -version

ENV MAVEN_HOME /usr/share/maven

# extension
RUN echo "**** install code-server extension ****" && \
    code-server --install-extension redhat.vscode-yaml &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension vscjava.vscode-java-pack

# 增加开发环境测试用例
COPY *.java /home/test/

