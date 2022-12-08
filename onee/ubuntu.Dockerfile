FROM quay.io/suisrc/vscode:1.73.1-cdr-ubuntu-one

USER root
RUN zsh -c "grep -rl open-vsx.org /vsc/**/*.js /vsc/**/*.json /vsc/**/*.map | xargs sed -i \
    -e 's|open-vsx.org/vscode/gallery|marketplace.visualstudio.com/_apis/public/gallery|g' \
    -e 's|open-vsx.org/vscode/item|marketplace.visualstudio.com/items|g' \
    -e 's|open-vsx.org/vscode/asset/{publisher}/{name}/{version}/Microsoft.VisualStudio.Code.WebResources/{path}|{publisher}.vscode-unpkg.net/{publisher}/{name}/{version}/{path}|g'"
USER vscode
# vscode extension
RUN code-server --install-extension golang.go &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension vscjava.vscode-java-pack@0.23.0 &&\
    code-server --install-extension vscjava.vscode-lombok &&\
    code-server --install-extension bungcip.better-toml &&\
    code-server --install-extension octref.vetur &&\
    rm -rf $USERDATA/CachedExtensionVSIXs/*