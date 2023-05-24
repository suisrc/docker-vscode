FROM ghcr.io/suisrc/webtop:0.0.1-base-debian

ARG VNC_RELEASE=1.1.0

# copy openresty resource
COPY --from=openresty /usr/local/openresty /usr/local/openresty
COPY --from=openresty /etc/nginx /etc/nginx
# COPY --from=openresty /var/run/openresty /var/run/openresty
# COPY --from=openresty /www /www -> /usr/local/openresty/nginx/html/

# update linux
RUN apt update && \
    DEBIAN_FRONTEND=noninteractive \
    apt install --no-install-recommends -y \
    dolphin \
    gwenview \
    kde-config-gtk-style \
    kdialog \
    kfind \
    khotkeys \
    kio-extras \
    knewstuff-dialog \
    konsole \
    ksysguard \
    kwin-addons \
    kwin-x11 \
    kwrite \
    plasma-desktop \
    plasma-workspace \
    qml-module-qt-labs-platform \
    systemsettings \
    pulseaudio \
    pulseaudio-utils \
    && \
    echo "**** desktop tweaks ****" && \
    sed -i \
    's/applications:org.kde.discover.desktop,/applications:org.kde.konsole.desktop,/g' \
    /usr/share/plasma/plasmoids/org.kde.plasma.taskmanager/contents/config/main.xml && \
    echo "**** cleanup ****" && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# fcitx5 fcitx5-pinyin??

COPY /root/ /

# install KasmVNC
# https://github.com/kasmtech/KasmVNC/releases
RUN VNC_URL="https://github.com/kasmtech/KasmVNC/releases/download/v${VNC_RELEASE}/kasmvncserver_bullseye_${VNC_RELEASE}_amd64.deb" && \
    curl -o /tmp/kasmvncserver.deb -L "${VNC_URL}" && \
    apt update && apt install -y /tmp/kasmvncserver.deb && \
    ln -s /usr/local/share/kasmvnc /usr/share/kasmvnc && \
    ln -s /usr/local/etc/kasmvnc /etc/kasmvnc && \
    ln -s /usr/local/lib/kasmvnc /usr/lib/kasmvncserver && \
    rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# # install msedge
# # ??替代 apt install chromium chromium-sandbox
# RUN if [ -z ${EDGE_RELEASE+x} ]; then \
#         EDGE_RELEASE=$(curl -q https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/ | grep href | grep .deb | sed 's/.*href="//g'  | cut -d '"' -f1 | sort --version-sort | tail -1); \
#     fi &&\
#     EDGE_URL="https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/$EDGE_RELEASE" &&\
#     curl -o /tmp/msedge.deb -L "${EDGE_URL}" &&\
#     apt update && apt install -y /tmp/msedge.deb &&\
#     rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# # sed -i 's|"\$@"| --no-sandbox  &|' /opt/microsoft/msedge/microsoft-edge
# # cp /usr/share/applications/microsoft-edge.desktop $HOME/Desktop/msedge.desktop

# # install vscode
# # ??替代  https://github.com/VSCodium/vscodium/releases/download/1.78.2.23132/codium_1.78.2.23132_amd64.deb
# RUN CODE_URL="https://update.code.visualstudio.com/latest/linux-deb-x64/stable" &&\
#     curl -o /tmp/vscode.deb -L "${CODE_URL}" &&\
#     apt update && apt install -y /tmp/vscode.deb &&\
#     rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*
# # sed -i 's#/usr/share/code/code#& --no-sandbox##' /usr/share/applications/code.desktop
# # cp /usr/share/applications/code.desktop $HOME/Desktop/vscode.desktop

# # USER $USERNAME
