# https://github.com/suisrc/docker-code-server/blob/master/debian.Dockerfile
# https://github.com/suisrc/docker-code-server/releases
# https://github.com/graalvm/graalvm-ce-builds/releases
# https://hub.docker.com/r/suisrc/vscode
# FROM suisrc/vscode:debian
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.65.2-debian

# args
ARG GRAALVM_RELEASE=vm-21.3.1
ARG JAVA_RELEASE=java8
ARG GRAALVM_URL

ARG MAVEN_RELEASE=3.8.5
ARG MAVEN_URL

USER root
# install oracle graalvm-ce 
RUN set -eux &&\
    if [ -z ${GRAALVM_URL+x} ]; then \
        if [ -z ${GRAALVM_RELEASE+x} ]; then \
            GRAALVM_RELEASE=$(curl -sX GET "https://api.github.com/repos/graalvm/graalvm-ce-builds/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        GRAALVM_URL="https://github.com/graalvm/graalvm-ce-builds/releases/download/${GRAALVM_RELEASE}/graalvm-ce-${JAVA_RELEASE}-linux-amd64-${GRAALVM_RELEASE##*-}.tar.gz"; \
        #GRAALVM_URL=$(curl -sX GET "https://api.github.com/repos/graalvm/graalvm-ce-builds/releases/tags/${GRAALVM_RELEASE}" \
        #    | jq -r '.assets[] | select(.browser_download_url | contains("graalvm-ce-java8-linux-amd64")) | .browser_download_url'); \
    fi &&\
    mkdir -p /graalvm &&\
    curl -fsSL --compressed ${GRAALVM_URL} -o graalvm-ce.tar.gz &&\
    tar -xzf graalvm-ce.tar.gz -C /graalvm --strip-components=1 &&\
    rm -f graalvm-ce.tar.gz

ENV PATH=/graalvm/bin:$PATH \
    JDK_HOME=/graalvm  \
    JAVA_HOME=/graalvm \
    MAVEN_HOME=/usr/share/maven
#RUN gu install native-image

# mvn
RUN if [ -z ${MAVEN_URL+x} ]; then \
        MAVEN_URL="https://downloads.apache.org/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz"; \
    fi &&\
    mkdir -p /usr/share/maven &&\
    curl -fsSL ${MAVEN_URL} -o apache-maven.tar.gz &&\
    tar -xzf apache-maven.tar.gz -C /usr/share/maven --strip-components=1 &&\
    sed -i -e "158d" -e "s/  <\/mirrors>/    -->\n&/g" /usr/share/maven/conf/settings.xml &&\
    rm -f apache-maven.tar.gz &&\
    ln -s /usr/share/maven/bin/mvn /usr/bin/mvn &&\
    mvn -version

USER vscode
# extension
RUN code-server --install-extension redhat.vscode-yaml &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension vscjava.vscode-java-pack &&\
    code-server --install-extension gabrielbb.vscode-lombok &&\
    rm -rf $USERDATA/CachedExtensionVSIXs/*
