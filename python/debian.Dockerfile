#FROM suisrc/vscode:1.44.2-01-centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.60.0-debian

RUN apt-get update && apt-get install -y python3 python3-pip &&\
    ln -s /usr/bin/python3 /usr/local/bin/py &&\
    apt-get autoremove -y && apt-get clean && rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# python extension
RUN pip3 install --upgrade pip &&\
    pip3 install --user pylint &&\
    pip3 install --user django
    #ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin

# vscode extension
RUN code-server --install-extension ms-python.python

