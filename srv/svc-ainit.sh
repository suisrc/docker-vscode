#!/bin/bash
# ============================================================================
# selkies.sh - 运行时初始化（kas svc-ainit 一次性任务）
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

echo "[svc-ainit] 运行时初始化..."

# 运行时目录（可能在 tmpfs 上, 每次启动需重建）
mkdir -p /run/dbus
mkdir -p /dev/shm
mkdir -p /var/run/kas
mkdir -p /home/webtop/Desktop
mkdir -p /home/webtop/.config/xfce4/xfconf/xfce-perchannel-xml

# ============================================================
# XFCE 默认配置（首次运行）
# ============================================================
if [ -d /defaults/xfce ]; then
    for f in xfce4-panel.xml xfwm4.xml xsettings.xml; do
        [ -f "/defaults/xfce/$f" ] && cp "/defaults/xfce/$f" /home/webtop/.config/xfce4/xfconf/xfce-perchannel-xml/ 2>/dev/null || true
    done
fi

# 权限
chown -R webtop:webtop /home/webtop
chown -R webtop:webtop /run/dbus 2>/dev/null || true

chown webtop:webtop /usr/share/selkies 2>/dev/null || true

# ============================================================
# 创建 selkies web 目录
# ============================================================
DASHBOARD="${DASHBOARD:-selkies-dashboard}"
HNAME=$(hostname)

if [ -d "/usr/share/selkies/${DASHBOARD}" ]; then
    rm -rf /usr/share/selkies/web
    cp -a "/usr/share/selkies/${DASHBOARD}" /usr/share/selkies/web
    echo "[svc-ainit] selkies web 从 ${DASHBOARD} 复制完成."
else
    echo "[svc-ainit] 警告: /usr/share/selkies/${DASHBOARD} 不存在，跳过 web 目录创建."
fi

# 图标
if [ -f /usr/share/selkies/www/icon.png ]; then
    cp /usr/share/selkies/www/icon.png /usr/share/selkies/web/icon.png 2>/dev/null || true
    cp /usr/share/selkies/www/icon.png /usr/share/selkies/web/favicon.ico 2>/dev/null || true
fi

# 动态生成 manifest.json
cat > /usr/share/selkies/web/manifest.json << MEOF
{
  "name": "${TITLE}-${HNAME}",
  "short_name": "${HNAME}",
  "manifest_version": 2,
  "version": "1.0.0",
  "display": "fullscreen",
  "background_color": "#000000",
  "theme_color": "#000000",
  "icons": [{ "src": "icon.png", "type": "image/png", "sizes": "180x180" }],
  "start_url": "/"
}
MEOF
echo "[svc-ainit] manifest.json 已生成，标题: ${TITLE}-${HNAME}"

chown -R webtop:webtop /usr/share/selkies/web 2>/dev/null || true

rm -f /etc/xdg/autostart/xscreensaver.desktop 2>/dev/null || true

echo "[svc-ainit] 初始化完成."

