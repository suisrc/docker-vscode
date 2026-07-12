#!/bin/bash
# ============================================================================
# svc-desktop - 桌面环境启动 (对齐 webtop svc-de)
# ============================================================================

export HOME=${HOME:-/home/webtop}
export DISPLAY=${DISPLAY:-:1}

unset DBUS_SESSION_BUS_ADDRESS

# 1. 等待 X 就绪
for i in $(seq 1 30); do
    su -s /bin/bash webtop -c "DISPLAY=${DISPLAY} xdpyinfo > /dev/null 2>&1" && break
    echo "等待 Xvfb... ($i)" && sleep 1
done

# 2. 等待 D-Bus
for i in $(seq 1 10); do
    [ -S /run/dbus/system_bus_socket ] && break
    sleep 0.5
done

# 3. 目录与权限
mkdir -p ${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml ${HOME}/Desktop
chown -R webtop:webtop ${HOME}

# 4. XFCE 默认配置
if [ ! -f "${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-panel.xml" ]; then
    [ -d /defaults/xfce ] && cp /defaults/xfce/* ${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml/ 2>/dev/null || true
fi

# 5. 分辨率设置（对齐 webtop svc-de）
RW=${SELKIES_MANUAL_WIDTH:-1920}
RH=${SELKIES_MANUAL_HEIGHT:-1080}
[ "$RW" = "0" ] && RW=1920
[ "$RH" = "0" ] && RH=1080
su -s /bin/bash webtop -c "DISPLAY=${DISPLAY} xrandr --output screen --mode ${RW}x${RH} --dpi 96" 2>/dev/null || true

# 6. Xresources 光标主题（对齐 webtop svc-de）
if [ ! -f "${HOME}/.Xresources" ]; then
    echo "Xcursor.theme: breeze_cursors" > ${HOME}/.Xresources
fi
chown webtop:webtop ${HOME}/.Xresources

# 7. 启动 XFCE（对齐 webtop startwm.sh）
exec su -s /bin/bash webtop -c "
    export HOME=${HOME} DISPLAY=${DISPLAY}
    unset DBUS_SESSION_BUS_ADDRESS
    exec dbus-run-session -- /usr/bin/xfce4-session > /dev/null 2>&1
"
