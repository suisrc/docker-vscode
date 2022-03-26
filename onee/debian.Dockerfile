FROM quay.io/suisrc/vscode:1.64.2-cdr-debian-one

# vscode extension
RUN code-server --install-extension MS-CEINTL.vscode-language-pack-zh-hans &&\
    code-server --install-extension golang.go &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension vscjava.vscode-java-pack &&\
    code-server --install-extension gabrielbb.vscode-lombok &&\
    code-server --install-extension bungcip.better-toml &&\
    rm -rf $USERDATA/CachedExtensionVSIXs/*
ADD locale.json /workspace/.local/share/code-server/User/argv.json