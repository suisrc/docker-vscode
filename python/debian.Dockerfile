#FROM suisrc/vscode:1.44.2-01-centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.65.2-debian

USER root
RUN apt update && apt install -y python3 python3-pip &&\
    ln -s /usr/bin/python3 /usr/local/bin/py &&\
    apt autoremove -y && apt clean &&\
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# python extension
RUN pip3 install --upgrade pip &&\
    pip3 install --user pylint &&\
    pip3 install --user django
    #ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin

USER vscode
# vscode extension
RUN code-server --install-extension ms-python.python &&\
    rm -rf $USERDATA/CachedExtensionVSIXs/*

