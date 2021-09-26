# FROM suisrc/vscode:debian
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.60.0-debian

# https://github.com/AdoptOpenJDK/openjdk11-binaries/releases
ARG JAVA_RELEASE=jdk-11.0.11+9_openj9-0.26.0
ARG JAVA_URL

ARG MAVEN_RELEASE=3.8.2
ARG MAVEN_URL

# jdk
RUN if [ -z ${JAVA_URL+x} ]; then \
        if [ -z ${JAVA_RELEASE+x} ]; then \
            JAVA_RELEASE=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk11-binaries/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        JAVA_URL=$(curl -sX GET "https://api.github.com/repos/AdoptOpenJDK/openjdk11-binaries/releases/tags/${JAVA_RELEASE}" \
            | jq -r 'first(.assets[] | select(.browser_download_url | contains("jdk_x64_linux_openj9_") and endswith(".tar.gz") ) | .browser_download_url)'); \
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
    sed -i -e "158d" -e "s/  <\/mirrors>/    -->\n&/g" /usr/share/maven/conf/settings.xml &&\
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


