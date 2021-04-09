#FROM suisrc/vscode:centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.54.2-centos

# args
ARG GRAALVM_RELEASE=vm-21.0.0.2
ARG JAVA_RELEASE=java11
ARG GRAALVM_URL

ARG MAVEN_RELEASE=3.8.1
ARG MAVEN_URL

# install oracle graalvm-ce 
RUN set -eux &&\
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

# mvn
RUN if [ -z ${MAVEN_URL+x} ]; then \
        MAVEN_URL="https://downloads.apache.org/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz"; \
    fi &&\
    mkdir -p /usr/share/maven &&\
    curl -fsSL ${MAVEN_URL} -o apache-maven.tar.gz &&\
    tar -xzf apache-maven.tar.gz -C /usr/share/maven --strip-components=1 &&\
    rm -f apache-maven.tar.gz &&\
    ln -s /usr/share/maven/bin/mvn /usr/bin/mvn &&\
    # smoke tests
    mvn -version

ENV MAVEN_HOME /usr/share/maven

# extension
RUN code-server --install-extension redhat.vscode-yaml &&\
    code-server --install-extension redhat.vscode-xml &&\
    #code-server --install-extension mhutchie.git-graph &&\
    #code-server --install-extension intellsmi.comment-translate &&\
    code-server --install-extension vscjava.vscode-java-pack &&\
    code-server --install-extension gabrielbb.vscode-lombok &&\
    code-server --install-extension sonarsource.sonarlint-vscode &&\
    code-server --install-extension cweijan.vscode-mysql-client2

