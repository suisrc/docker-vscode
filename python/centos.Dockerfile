#FROM suisrc/vscode:1.44.2-01-centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:centos

RUN yum update -y && yum install -y gcc libz-dev python3 &&\
    ln -s /usr/bin/python3 /usr/local/bin/py &&\
    rm -rf /tmp/* /var/tmp/* /var/cache/yum

# sqlite版本低, 无法使用django
RUN curl -L https://www.sqlite.org/2020/sqlite-autoconf-3310100.tar.gz -o sqlite-autoconf.tar.gz &&\
    mkdir sqlite-autoconf &&\
    tar -zxvf sqlite-autoconf.tar.gz -C sqlite-autoconf --strip-components=1 &&\
    cd    sqlite-autoconf &&\
    ./configure --prefix=/usr/local &&\
    make && make install &&\
    cd .. && rm -rf sqlite-autoconf &&\
    mv /usr/bin/sqlite3  /usr/bin/sqlite3_old &&\
    ln -s /usr/local/bin/sqlite3   /usr/bin/sqlite3 &&\
    echo "/usr/local/lib" > /etc/ld.so.conf.d/sqlite3.conf &&\
    ldconfig &&\
    sqlite3 -version

# python extension
RUN pip3 install --user pylint &&\
    pip3 install --user django &&\
    ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin

# vscode extension
RUN code-server --install-extension ms-python.python

