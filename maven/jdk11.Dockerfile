FROM debian:buster-slim

# set version label
LABEL maintainer="suisrc@outlook.com"

ENV container docker
#ARG LINUX_MIRRORS=http://mirrors.aliyun.com
# linux and softs
RUN if [ ! -z ${LINUX_MIRRORS+x} ]; then \
        mv /etc/apt/sources.list /etc/apt/sources.list.bak && \
        echo "deb ${LINUX_MIRRORS}/debian/ buster main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian/ buster main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb ${LINUX_MIRRORS}/debian-security buster/updates main" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian-security buster/updates main" >>/etc/apt/sources.list &&\
        echo "deb ${LINUX_MIRRORS}/debian/ buster-updates main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian/ buster-updates main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb ${LINUX_MIRRORS}/debian/ buster-backports main non-free contrib" >>/etc/apt/sources.list &&\
        echo "deb-src ${LINUX_MIRRORS}/debian/ buster-backports main non-free contrib" >>/etc/apt/sources.list; \
    fi &&\
    apt-get update && \
    apt-get install --no-install-recommends -y \
        ca-certificates curl git jq dpkg \
        gcc build-essential libz-dev zlib1g-dev &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

ENV JAVA_VERSION=11 \
    GRAALVM_VERSION=22.0.0.2 \
    MAVEN_VERSION=3.8.5 \
    JDK_HOME=/usr/local/graal \
    JAVA_HOME=/usr/local/graal \
    MAVEN_HOME=/usr/local/maven \
    PATH=/usr/local/graal/bin:/usr/local/maven/bin:$PATH

# install oracle graalvm-ce 
# https://github.com/graalvm/graalvm-ce-builds/releases/download/vm-22.0.0.2/graalvm-ce-java11-linux-amd64-22.0.0.2.tar.gz
RUN set -eux &&\
    if [ -z ${GRAALVM_URL+x} ]; then \
        if [ -z ${GRAALVM_RELEASE+x} ]; then \
            GRAALVM_RELEASE=$(curl -sX GET "https://api.github.com/repos/graalvm/graalvm-ce-builds/releases/latest" \
            | awk '/tag_name/{print $4;exit}' FS='[""]'); \
        fi && \
        GRAALVM_URL="https://github.com/graalvm/graalvm-ce-builds/releases/download/vm-${GRAALVM_VERSION}/graalvm-ce-java${JAVA_VERSION}-linux-amd64-${GRAALVM_VERSION}.tar.gz"; \
        #GRAALVM_URL=$(curl -sX GET "https://api.github.com/repos/graalvm/graalvm-ce-builds/releases/tags/${GRAALVM_RELEASE}" \
        #    | jq -r '.assets[] | select(.browser_download_url | contains("graalvm-ce-java11-linux-amd64")) | .browser_download_url'); \
        # https://github.com/graalvm/graalvm-ce-builds/releases/download/vm-20.0.0/graalvm-ce-java11-linux-amd64-20.0.0.tar.gz
    fi &&\
    mkdir /usr/local/graal &&\
    curl -fsSL --compressed ${GRAALVM_URL} |\
    tar -xz -C /usr/local/graal --strip-components=1 &&\
    java -version
    #curl `#--fail --silent --location --retry 3` -fSL ${GRAALVM_URL} | tar -zxC /graalvm --strip-components=1 &&\

# native
RUN gu install native-image

# maven
RUN if [ -z ${MAVEN_URL+x} ]; then \
        MAVEN_URL="https://downloads.apache.org/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz"; \
    fi &&\
    mkdir /usr/local/maven &&\
    curl -fSL --compressed ${MAVEN_URL} | \
    tar -xz -C /usr/local/maven --strip-components=1 &&\
    sed -i -e "158d" -e "s/  <\/mirrors>/    -->\n&/g" /usr/local/maven/conf/settings.xml &&\
    mvn -version

# settings.xml -> /root/.m2/settings.xml
RUN mkdir /data
WORKDIR   /data
CMD  [ "mvn", "-version"]
#CMD [ "mvn", "package", "-Pnative"]