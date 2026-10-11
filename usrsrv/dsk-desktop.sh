#!/bin/bash
# ============================================================================
# dsk-desktop - 会话启动 (两种模式)
#
# - 桌面模式 (默认): dbus-run-session -- xfce4-session, 对齐上游 startwm.sh
# - 单应用模式 (DSK_APP 设置时): 极简 WM (openbox) + 唯一应用 + rc.xml 强制
#   最大化, 对齐上游 baseimage-selkies 单应用镜像 (SELKIES_DESKTOP=false):
#   一个系统只为一个应用服务 —— 应用退出即会话结束 (session bus 随之销毁),
#   由 kas autorestart 重新拉起; system bus/pulseaudio/Xvfb/selkies 不变
# ============================================================================

. /usr/srv/dsk-lib.sh
dsk_prepare_runtime

export DISPLAY=${DISPLAY:-:1}
export XCURSOR_THEME=breeze_cursors

# 隔离容器环境可能注入的 session bus 地址, 由 dbus-run-session 新建
unset DBUS_SESSION_BUS_ADDRESS

# 1. 等待 X 就绪
for i in $(seq 1 30); do
    run_as_user xdpyinfo > /dev/null 2>&1 && break
    echo "等待 Xvfb... ($i)" && sleep 1
done

# 2. 等待 system bus
for i in $(seq 1 20); do
    [ -S /run/dbus/system_bus_socket ] && break
    sleep 0.5
done

# 3. 目录
mkdir -p "${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml" "${HOME}/Desktop"

# 4. XFCE 默认配置 (缺失才拷贝, 不覆盖用户已有配置)
CONF_DIR="${HOME}/.config/xfce4/xfconf/xfce-perchannel-xml"
if [ ! -f "${CONF_DIR}/xfce4-panel.xml" ] && [ -d /defaults/xfce ]; then
    cp /defaults/xfce/*.xml "${CONF_DIR}/" 2>/dev/null || true
    if [ "${DSK_USER}" != "root" ]; then
        chown -R "${DSK_USER}:" "${HOME}/.config"
    fi
fi

# 5. 分辨率: 仅在显式指定 SELKIES_MANUAL_WIDTH/HEIGHT 时设置。
#    Xvfb framebuffer 之外的模式不存在, 需按上游 svc-de 的三段式:
#    cvt 生成 modeline -> xrandr --newmode/--addmode --output --mode
#    (原先直接 --mode 在 mode 不存在时会静默失败)
if [ -n "${SELKIES_MANUAL_WIDTH}" ] || [ -n "${SELKIES_MANUAL_HEIGHT}" ]; then
    RW=${SELKIES_MANUAL_WIDTH:-1024}
    RH=${SELKIES_MANUAL_HEIGHT:-768}
    [ "${RW}" = "0" ] && RW=1024
    [ "${RH}" = "0" ] && RH=768
    MODELINE=$(run_as_user cvt "${RW}" "${RH}" 2>/dev/null | grep "Modeline" | sed 's/^.*Modeline //')
    MODELINE_ARGS=$(echo "${MODELINE}" | tr -d '"')
    MODELINE_NAME=$(echo "${MODELINE_ARGS}" | awk '{print $1}')
    if [ -n "${MODELINE_NAME}" ]; then
        if ! run_as_user xrandr 2>/dev/null | grep -q "${MODELINE_NAME}"; then
            run_as_user xrandr --newmode ${MODELINE_ARGS} 2>/dev/null || true
            run_as_user xrandr --addmode screen "${MODELINE_NAME}" 2>/dev/null || true
        fi
        run_as_user xrandr --output screen --mode "${MODELINE_NAME}" --dpi 96 2>/dev/null || true
    fi
fi

# 6. Xresources 光标主题 (xrdb 加载后 X 应用才会应用, 仅写文件无效)
if [ ! -f "${HOME}/.Xresources" ]; then
    echo "Xcursor.theme: breeze_cursors" > ${HOME}/.Xresources
    if [ "${DSK_USER}" != "root" ]; then
        chown "${DSK_USER}:" ${HOME}/.Xresources
    fi
fi
run_as_user xrdb -merge "${HOME}/.Xresources" 2>/dev/null || true

# 7. 启动会话
cd "${HOME}" 2>/dev/null || true
if [ -n "${DSK_APP}" ]; then
    # ============================================================
    # 单应用模式: openbox + 唯一应用
    # ============================================================
    OB_DIR="${HOME}/.config/openbox"
    mkdir -p "${OB_DIR}"
    # 应用规则: 所有窗口强制最大化且无装饰 (等价上游对 rc.xml 的 sed 注入)
    cat > "${OB_DIR}/rc.xml" <<'OBEOF'
<?xml version="1.0" encoding="UTF-8"?>
<openbox_config xmlns="http://openbox.org/3.4/xrc">
  <applications>
    <application class="*">
      <maximized>yes</maximized>
      <decor>no</decor>
    </application>
  </applications>
</openbox_config>
OBEOF
    if [ "${DSK_USER}" != "root" ]; then
        chown "${DSK_USER}:" "${OB_DIR}/rc.xml"
    fi
    echo "[dsk-desktop] 单应用模式: ${DSK_APP}"
    export DSK_APP
    exec_as_user dbus-run-session -- /bin/bash -c '
        openbox &
        OB_PID=$!
        # 等 WM 就绪后再拉应用, 保证最大化规则生效
        for _ in $(seq 1 10); do
            xprop -root _NET_SUPPORTING_WM_CHECK > /dev/null 2>&1 && break
            sleep 0.5
        done
        ${DSK_APP} &
        APP_PID=$!
        wait "${APP_PID}"
        kill "${OB_PID}" 2>/dev/null || true
    ' > /dev/null 2>&1
else
    # ============================================================
    # 桌面模式: XFCE
    # ============================================================
    exec_as_user dbus-run-session -- /usr/bin/xfce4-session > /dev/null 2>&1
fi
