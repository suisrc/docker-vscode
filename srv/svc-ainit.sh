#!/bin/bash
# ============================================================================
# selkies.sh - 运行时初始化（kas init-setup 一次性任务）
#
# 镜像链: base → bvsc → 本层(Dockerfile)
#   Dockerfile 已在构建时完成:
#     - apt 安装 X11/XFCE/桌面栈
#     - COPY --from=webtop:debian-xfce 获取 /lsiopy + /usr/share/selkies + /defaults
#     - 创建基础目录和权限
#
#   本脚本只做运行时必须的初始化（tmpfs 目录、动态权限等）
# ============================================================================

set -e

echo "[init-setup] 运行时初始化..."

# 运行时目录（可能在 tmpfs 上, 每次启动需重建）
mkdir -p /run/dbus
mkdir -p /dev/shm
mkdir -p /var/run/kas
mkdir -p /home/webtop/Desktop
mkdir -p /home/webtop/.config/xfce4/xfconf/xfce-perchannel-xml

# XFCE 默认配置（首次运行）
if [ -d /defaults/xfce ]; then
    for f in xfce4-panel.xml xfwm4.xml xsettings.xml; do
        [ -f "/defaults/xfce/$f" ] && cp "/defaults/xfce/$f" /home/webtop/.config/xfce4/xfconf/xfce-perchannel-xml/ 2>/dev/null || true
    done
fi

# 权限
chown -R webtop:webtop /home/webtop
chown -R webtop:webtop /run/dbus 2>/dev/null || true

chown webtop:webtop /usr/share/selkies 2>/dev/null || true

# 标题设为主机名
HNAME=$(hostname)
if [ -f /usr/share/selkies/web/manifest.json ]; then
    sed -i "s/\"name\": \".*\"/\"name\": \"${HNAME}\"/" /usr/share/selkies/web/manifest.json
    sed -i "s/\"short_name\": \".*\"/\"short_name\": \"${HNAME}\"/" /usr/share/selkies/web/manifest.json
    echo "[init-setup] 标题已设为: ${HNAME}"
fi

rm -f /etc/xdg/autostart/xscreensaver.desktop 2>/dev/null || true

echo "[init-setup] 初始化完成."

# 创建 chromium 包装器（绕过容器 seccomp 限制）
if [ ! -f /usr/local/bin/wrapped-chromium ]; then
    cat > /usr/local/bin/wrapped-chromium << 'CEOF'
#!/bin/bash
BIN=/usr/bin/chromium
if ! pgrep -x chromium > /dev/null 2>&1; then
    rm -f $HOME/.config/chromium/Singleton* 2>/dev/null || true
fi
exec ${BIN} \
    --password-store=basic \
    --no-sandbox \
    --test-type \
    --disable-dev-shm-usage \
    --disable-features=UseClone3ForSandbox \
    --ozone-platform=x11 \
    "$@"
CEOF
    chmod +x /usr/local/bin/wrapped-chromium
    echo "[init-setup] wrapped-chromium 已创建."
fi

# 修复 chromium desktop 入口指向 wrapped-chromium
if [ -f /usr/share/applications/chromium.desktop ]; then
    sed -i 's#^Exec=/usr/bin/chromium#Exec=/usr/local/bin/wrapped-chromium#g' /usr/share/applications/chromium.desktop
    # 创建桌面快捷方式
    cp /usr/share/applications/chromium.desktop /home/webtop/Desktop/
    chown webtop:webtop /home/webtop/Desktop/chromium.desktop
    chmod +x /home/webtop/Desktop/chromium.desktop
    echo "[init-setup] chromium 桌面入口已修复."
fi

