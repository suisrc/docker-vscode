#FROM suisrc/vscode:centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.54.2-centos

RUN yum update -y && yum install -y python3 &&\
    ln -s /usr/bin/python3 /usr/local/bin/py &&\
    rm -rf /tmp/* /var/tmp/* /var/cache/yum

# python extension
RUN pip3 install --upgrade pip &&\
    pip3 install --user pylint &&\
    pip3 install --user django
    #ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin

# vscode extension
RUN code-server --install-extension ms-python.python

