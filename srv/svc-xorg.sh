#!/bin/bash
# ============================================================================
# svc-xorg - Xvfb 虚拟显示服务 (以 webtop 用户运行)
# ============================================================================

export DISPLAY=${DISPLAY:-:1}

# Cleanup previous lock
rm -f /tmp/.X1-lock

# 默认虚拟分辨率
DEFAULT_RES="${MAX_RES:-1920x1080}"

if [ -n "${SELKIES_MANUAL_HEIGHT}" ] || [ -n "${SELKIES_MANUAL_WIDTH}" ]; then
    T_WIDTH="${SELKIES_MANUAL_WIDTH:-1024}"
    T_HEIGHT="${SELKIES_MANUAL_HEIGHT:-768}"
    [ "${T_WIDTH}" = "0" ] && T_WIDTH="1024"
    [ "${T_HEIGHT}" = "0" ] && T_HEIGHT="768"
    DEFAULT_RES="${T_WIDTH}x${T_HEIGHT}"
fi

exec su -s /bin/bash webtop -c "DISPLAY=${DISPLAY} /usr/bin/Xvfb ${DISPLAY} \
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
