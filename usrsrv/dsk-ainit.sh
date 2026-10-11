#!/bin/bash
# ============================================================================
# dsk-ainit - 运行时初始化（kas 一次性任务, 每次容器启动执行一次）
#
# 镜像链: base → xfce → desk
#   构建时已完成: apt 桌面栈安装, selkies/defaults 拷贝 (Dockerfile.xfce)
#
#   本脚本只做运行时必须的初始化:
#     - 运行时目录
#     - XFCE 默认配置 (缺失才拷贝, 不覆盖用户已有配置)
#     - selkies web 前端 (fix-selkies.sh)
#   /run/dbus 由 dsk-dbus 负责; HOME 属主由 init.sh 每次启动负责
# ============================================================================

set -e

. /usr/srv/dsk-lib.sh

echo "[dsk-ainit] 运行时初始化... (USER=${DSK_USER}, HOME=${DSK_HOME})"

mkdir -p /var/run/kas
mkdir -p "${DSK_HOME}/Desktop"
mkdir -p "${DSK_HOME}/.config/xfce4/xfconf/xfce-perchannel-xml"

# ============================================================
# XFCE 默认配置（首次运行, 缺失才拷贝）
# ============================================================
CONF_DIR="${DSK_HOME}/.config/xfce4/xfconf/xfce-perchannel-xml"
if [ ! -f "${CONF_DIR}/xfce4-panel.xml" ] && [ -d /defaults/xfce ]; then
    cp /defaults/xfce/*.xml "${CONF_DIR}/" 2>/dev/null || true
    echo "[dsk-ainit] XFCE 默认配置已初始化."
fi

# 权限 (非 root 用户才 chown)
if [ "${DSK_USER}" != "root" ]; then
    chown -R "${DSK_USER}:" "${DSK_HOME}/.config" "${DSK_HOME}/Desktop" 2>/dev/null || true
fi

rm -f /etc/xdg/autostart/xscreensaver.desktop 2>/dev/null || true

# ============================================================
# selkies web 前端（dashboard 选择/图标/manifest）
# ============================================================
bash /usr/srv/fix-selkies.sh
if [ "${DSK_USER}" != "root" ]; then
    chown -R "${DSK_USER}:" /usr/share/selkies/web 2>/dev/null || true
fi

echo "[dsk-ainit] 初始化完成."
