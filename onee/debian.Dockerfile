FROM quay.io/suisrc/vscode:1.66.1-debian-one

ENV USERDATA=/workspace/.openvscode-server/data
# vscode extension
RUN code-server --install-extension golang.go &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension vscjava.vscode-java-pack &&\
    code-server --install-extension gabrielbb.vscode-lombok &&\
    code-server --install-extension bungcip.better-toml &&\
    code-server --install-extension octref.vetur &&\
    rm -rf $USERDATA/CachedExtensionVSIXs/*