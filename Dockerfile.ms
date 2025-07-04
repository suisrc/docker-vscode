## x. ms 依赖的 vscode
ARG BASE_IMAGE_TAG

FROM ghcr.io/suisrc/webtop:${BASE_IMAGE_TAG}
# ================================================================

RUN zsh -c "grep -rl open-vsx.org /vsc/**/*.js /vsc/**/*.json /vsc/**/*.map | xargs sed -i \
    -e 's|open-vsx.org/vscode/gallery|marketplace.visualstudio.com/_apis/public/gallery|g' \
    -e 's|open-vsx.org/vscode/item|marketplace.visualstudio.com/items|g' \
    -e 's|open-vsx.org/vscode/asset/{publisher}/{name}/{version}/Microsoft.VisualStudio.Code.WebResources/{path}|{publisher}.vscode-unpkg.net/{publisher}/{name}/{version}/{path}|g'" \
    || true

# vscode extension
RUN code-server --install-extension golang.go &&\
    code-server --install-extension Vue.volar &&\
    code-server --install-extension ms-python.python &&\
    code-server --install-extension vscjava.vscode-java-pack &&\
    code-server --install-extension vscjava.vscode-lombok &&\
    code-server --install-extension redhat.vscode-xml &&\
    code-server --install-extension bungcip.better-toml &&\
    rm -rf /tmp/* /var/tmp/* $HOME/.local/share/code-server/CachedExtensionVSIXs/* $HOME/.openvscode-server/data/CachedExtensionVSIXs/*

