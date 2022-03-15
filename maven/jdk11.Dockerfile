FROM debian:buster-slim
# FROM debian:buster
# args
ARG GRAALVM_RELEASE=vm-22.0.0.2
ARG JAVA_RELEASE=java11
ARG GRAALVM_URL

ARG MAVEN_RELEASE=3.6.3
ARG MAVEN_URL

#ARG LINUX_MIRRORS=http://mirrors.aliyun.com

# set version label
LABEL maintainer="suisrc@outlook.com"

ENV container docker
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
    #curl `#--fail --silent --location --retry 3` -fSL ${GRAALVM_URL} | tar -zxC /graalvm --strip-components=1
    curl -fsSL --compressed ${GRAALVM_URL} | tar -zxC /graalvm --strip-components=1

ENV PATH=/graalvm/bin:$PATH
RUN gu install native-image

ENV JDK_HOME=/graalvm
ENV JAVA_HOME=/graalvm

# mvn
RUN if [ -z ${MAVEN_URL+x} ]; then \
        MAVEN_URL="https://downloads.apache.org/maven/maven-3/${MAVEN_RELEASE}/binaries/apache-maven-${MAVEN_RELEASE}-bin.tar.gz"; \
    fi &&\
    mkdir -p /usr/share/maven &&\
    curl -fsSL ${MAVEN_URL} | tar -zxC /usr/share/maven --strip-components=1 &&\
    ln -s /usr/share/maven/bin/mvn /usr/bin/mvn &&\
    mvn -version

ENV MAVEN_HOME /usr/share/maven
# settings.xml -> /root/.m2/settings.xml

RUN mkdir /data
WORKDIR   /data
#CMD [ "mvn", "package", "-Pnative"]
CMD  [ "mvn", "-version"]