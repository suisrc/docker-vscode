#!/bin/bash
# ============================================================================
# svc-desktop - 桌面环境启动 (根据 ${USER:-} 运行时态)
# ============================================================================

# 运行时用户态: 默认 root, 可通过环境变量 USER 覆盖 (root 或自定义用户名)
USER="${USER:-root}"
if [ "$USER" = "root" ]; then
    HOME_DIR="/root"
else
    HOME_DIR="/home/${USER}"
fi

export HOME="${HOME_DIR}"
export DISPLAY=${DISPLAY:-:1}

unset DBUS_SESSION_BUS_ADDRESS

# 辅助函数: 以目标用户身份运行命令
_run_as_user() {
    if [ "$USER" = "root" ]; then
        eval "$1"
    else
        su -s /bin/bash "${USER}" -c "$1"
    fi
}

# 1. 等待 X 就绪
for i in $(seq 1 30); do
    _run_as_user "DISPLAY=${DISPLAY} xdpyinfo > /dev/null 2>&1" && break
    echo "等待 Xvfb... ($i)" && sleep 1
done

# 2. 等待 D-Bus
for i in $(seq 1 10); do
    [ -S /run/dbus/system_bus_socket ] && break
    sleep 0.5
done

# 3. 目录与权限
mkdir -p ${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml ${HOME}/Desktop
if [ "$USER" != "root" ]; then
    chown -R "${USER}:${USER}" ${HOME}
fi

# 4. XFCE 默认配置
if [ ! -f "${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml" ]; then
    [ -d /defaults/xfce ] && cp /defaults/xfce/* ${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml/ 2>/dev/null || true
fi

# 5. 分辨率设置
RW=${SELKIES_MANUAL_WIDTH:-1920}
RH=${SELKIES_MANUAL_HEIGHT:-1080}
[ "$RW" = "0" ] && RW=1920
[ "$RH" = "0" ] && RH=1080
_run_as_user "DISPLAY=${DISPLAY} xrandr --output screen --mode ${RW}x${RH} --dpi 96" 2>/dev/null || true

# 6. Xresources 光标主题
if [ ! -f "${HOME}/.Xresources" ]; then
    echo "Xcursor.theme: breeze_cursors" > ${HOME}/.Xresources
fi
if [ "$USER" != "root" ]; then
    chown "${USER}:${USER}" ${HOME}/.Xresources
fi

# 7. 启动 XFCE
if [ "$USER" = "root" ]; then
    export HOME="${HOME_DIR}" DISPLAY="${DISPLAY}"
    unset DBUS_SESSION_BUS_ADDRESS
    exec dbus-run-session -- /usr/bin/xfce4-session > /dev/null 2>&1
else
    exec su -s /bin/bash "${USER}" -c "
        export HOME=${HOME_DIR} DISPLAY=${DISPLAY}
        unset DBUS_SESSION_BUS_ADDRESS
        exec dbus-run-session -- /usr/bin/xfce4-session > /dev/null 2>&1
    "
fi
