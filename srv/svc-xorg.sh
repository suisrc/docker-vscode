#!/bin/bash
# ============================================================================
# svc-xorg - Xvfb 虚拟显示服务 (根据 ${USER:-} 运行时态运行)
# ============================================================================

# 运行时用户态: 默认 root, 可通过环境变量 USER 覆盖 (root 或自定义用户名)
USER="${USER:-root}"

export DISPLAY=${DISPLAY:-:1}

# Cleanup previous lock
rm -f /tmp/.X1-lock

# 默认虚拟分辨率（大尺寸避免 selkies resize 时触发 XShm 重初始化崩溃）
DEFAULT_RES="${MAX_RES:-15360x8640}"

if [ -n "${SELKIES_MANUAL_HEIGHT}" ] || [ -n "${SELKIES_MANUAL_WIDTH}" ]; then
    T_WIDTH="${SELKIES_MANUAL_WIDTH:-1024}"
    T_HEIGHT="${SELKIES_MANUAL_HEIGHT:-768}"
    [ "${T_WIDTH}" = "0" ] && T_WIDTH="1024"
    [ "${T_HEIGHT}" = "0" ] && T_HEIGHT="768"
    DEFAULT_RES="${T_WIDTH}x${T_HEIGHT}"
fi

if [ "$USER" = "root" ]; then
    exec /usr/bin/Xvfb ${DISPLAY} \
        -screen 0 ${DEFAULT_RES}x24 \
        -dpi 96 \
        +extension COMPOSITE \
        +extension DAMAGE \
        +extension GLX \
        +extension RANDR \
        +extension RENDER \
        +extension MIT-SHM \
        +extension XFIXES \
        +extension XTEST \
        +iglx \
        +render \
        -nolisten tcp \
        -ac \
        -noreset \
        -shmem
else
    exec su -s /bin/bash "${USER}" -c "DISPLAY=${DISPLAY} /usr/bin/Xvfb ${DISPLAY} \
        -screen 0 ${DEFAULT_RES}x24 \
        -dpi 96 \
        +extension COMPOSITE \
        +extension DAMAGE \
        +extension GLX \
        +extension RANDR \
        +extension RENDER \
        +extension MIT-SHM \
        +extension XFIXES \
        +extension XTEST \
        +iglx \
        +render \
        -nolisten tcp \
        -ac \
        -noreset \
        -shmem"
fi
