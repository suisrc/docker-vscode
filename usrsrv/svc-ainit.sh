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

# 运行时用户态: 默认 root, 可通过环境变量 USER 覆盖 (root 或自定义用户名)
USER="${USER:-root}"
if [ "$USER" = "root" ]; then
    HOME_DIR="/root"
else
    HOME_DIR="/home/${USER}"
fi

echo "[svc-ainit] 运行时初始化... (USER=${USER}, HOME=${HOME_DIR})"

# 运行时目录（可能在 tmpfs 上, 每次启动需重建）
mkdir -p /run/dbus
mkdir -p /dev/shm
mkdir -p /var/run/kas
mkdir -p "${HOME_DIR}/Desktop"
mkdir -p "${HOME_DIR}/.config/xfce4/xfconf/xfce-perchannel-xml"

# ============================================================
# XFCE 默认配置（首次运行）
# ============================================================
if [ -d /defaults/xfce ]; then
    for f in xfce4-panel.xml xfwm4.xml xsettings.xml; do
        [ -f "/defaults/xfce/$f" ] && cp "/defaults/xfce/$f" "${HOME_DIR}/.config/xfce4/xfconf/xfce-perchannel-xml/" 2>/dev/null || true
    done
fi

# 权限 (非 root 用户才 chown)
if [ "$USER" != "root" ]; then
    chown -R "${USER}:${USER}" "${HOME_DIR}"
    chown -R "${USER}:${USER}" /run/dbus 2>/dev/null || true
fi

rm -f /etc/xdg/autostart/xscreensaver.desktop 2>/dev/null || true

# ============================================================
# COPY selkies web（首次运行）
# ============================================================

bash /usr/srv/web-selkies.sh
if [ "$USER" != "root" ]; then
    chown -R "${USER}:${USER}" /usr/share/selkies/web 2>/dev/null || true
fi

echo "[svc-ainit] 初始化完成."

