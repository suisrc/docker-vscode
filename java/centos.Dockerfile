#FROM suisrc/vscode:centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.54.2-centos

# https://github.com/AdoptOpenJDK/openjdk8-binaries/releases
ARG JAVA_RELEASE=jdk8u282-b08_openj9-0.24.0
ARG JAVA_URL

ARG MAVEN_RELEASE=3.8.1
ARG MAVEN_URL

# jdk
RUN if [ -z ${JAVA_URL+x} ]; then \
        if [ -z ${JAVA_RELEASE+x} ]; then \
            JAVA_RELEASE=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk8-binaries/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        JAVA_URL=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk8-binaries/releases/tags/${JAVA_RELEASE}" \
            | jq -r 'first(.assets[] | select(.browser_download_url | contains("jdk_x64_linux_openj9_linuxXL") and endswith(".tar.gz") ) | .browser_download_url)'); \
        # jdk8u252-b09.1 not has linux package
        # JAVA_URL="https://github.com/AdoptOpenJDK/openjdk8-binaries/releases/download/jdk8u252-b09/OpenJDK8U-jdk_x64_linux_hotspot_8u252b09.tar.gz"; \
    fi &&\
    mkdir -p /usr/lib/jvm/java-adopt &&\
    curl -L ${JAVA_URL} -o /tmp/adopt-open-jdk.tar.gz &&\
    tar -xzf /tmp/adopt-open-jdk.tar.gz -C /usr/lib/jvm/java-adopt --strip-components=1 &&\
    #ln -s /usr/lib/jvm/java-adopt/bin/java /usr/bin/java &&\
    rm -rf /tmp/*
    # smoke tests
    # java -version

ENV PATH=/usr/lib/jvm/java-adopt/bin:$PATH
ENV JDK_HOME=/usr/lib/jvm/java-adopt
ENV JAVA_HOME=/usr/lib/jvm/java-adopt

# mvn
# http://mirrors.tuna.tsinghua.edu.cn/apache/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz"
# https://downloads.apache.org/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz
RUN if [ -z ${MAVEN_URL+x} ]; then \
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
RUN code-server --install-extension redhat.vscode-yaml &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension vscjava.vscode-java-pack &&\
    code-server --install-extension gabrielbb.vscode-lombok &&\
    code-server --install-extension sonarsource.sonarlint-vscode &&\
    code-server --install-extension cweijan.vscode-mysql-client2


