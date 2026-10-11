#!/bin/bash
# ============================================================================
# dsk-xorg - Xvfb 虚拟显示服务
#
# 纯 X11 模式 (与上游 webtop debian-xfce 一致): Xvfb 即 X server, 没有
# Wayland 合成器, Xwayland 不参与本模式。
# 容器透传 GPU (/dev/dri/renderD*) 时启用 DRI3 glamor 硬件加速, 对齐上游
# svc-xorg; DISABLE_DRI3=true 可强制关闭。
# ============================================================================

. /usr/srv/dsk-lib.sh

export DISPLAY=${DISPLAY:-:1}
dsk_prepare_runtime

# 清理上次残留的锁文件 (DISPLAY=:1 -> /tmp/.X1-lock)
X_NUM="${DISPLAY#:}"; X_NUM="${X_NUM%%.*}"
rm -f "/tmp/.X${X_NUM}-lock"

# 默认虚拟分辨率（大尺寸避免 selkies resize 时触发 XShm 重初始化崩溃）
DEFAULT_RES="${MAX_RES:-15360x8640}"

if [ -n "${SELKIES_MANUAL_HEIGHT}" ] || [ -n "${SELKIES_MANUAL_WIDTH}" ]; then
    T_WIDTH="${SELKIES_MANUAL_WIDTH:-1024}"
    T_HEIGHT="${SELKIES_MANUAL_HEIGHT:-768}"
    [ "${T_WIDTH}" = "0" ] && T_WIDTH="1024"
    [ "${T_HEIGHT}" = "0" ] && T_HEIGHT="768"
    DEFAULT_RES="${T_WIDTH}x${T_HEIGHT}"
fi

# 有 render node 且本机 Xvfb 支持 -glamor 时启用 DRI3 (对齐上游 VFBCOMMAND)
VFBCOMMAND=""
if [ "${DISABLE_DRI3:-false}" != "true" ]; then
    for node in /dev/dri/renderD*; do
        [ -e "${node}" ] || continue
        if /usr/bin/Xvfb -help 2>&1 | grep -q -- '-glamor'; then
            VFBCOMMAND="-glamor -dri ${node}"
            echo "[dsk-xorg] Xvfb 启用 DRI3: ${node}"
        fi
        break
    done
fi

# 清掉会干扰软件渲染路径的 Zink 环境变量 (对齐上游)
unset GALLIUM_DRIVER MESA_LOADER_DRIVER_OVERRIDE LIBGL_KOPPER_DRI2

XVFB_ARGS=(
    "${DISPLAY}"
    -screen 0 "${DEFAULT_RES}x24"
    -dpi 96
    +extension COMPOSITE
    +extension DAMAGE
    +extension GLX
    +extension RANDR
    +extension RENDER
    +extension MIT-SHM
    +extension XFIXES
    +extension XTEST
    +iglx
    +render
    -nolisten tcp
    -ac
    -noreset
    -shmem
)
[ -n "${VFBCOMMAND}" ] && XVFB_ARGS+=(${VFBCOMMAND})

exec_as_user /usr/bin/Xvfb "${XVFB_ARGS[@]}"
