#!/bin/bash
# ============================================================================
# svc-selkies - Selkies WebRTC/WebSocket 桥接服务
# 监听 localhost:8081 提供 WebSocket 接口（selkies 默认端口）
# 根据 ${USER:-} 运行时态运行
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
export SELKIES_ENABLE_RESIZE=true

# 等待 X 服务就绪
for i in $(seq 1 30); do
    if [ "$USER" = "root" ]; then
        DISPLAY=${DISPLAY} xdpyinfo -display ${DISPLAY} > /dev/null 2>&1 && break
    else
        su -s /bin/bash "${USER}" -c "DISPLAY=${DISPLAY} xdpyinfo -display ${DISPLAY} > /dev/null 2>&1" && break
    fi
    echo "等待 Xvfb 就绪... ($i)"
    sleep 1
done

# 设置光标主题
export XCURSOR_THEME=breeze_cursors

# PulseAudio 音频 (selkies 需要 PULSE_RUNTIME_PATH 才能连接 pulseaudio)
USER_UID=$(id -u "${USER}")
export PULSE_RUNTIME_PATH=/run/user/${USER_UID}/pulse

if [ "$USER" = "root" ]; then
    exec env HOME="${HOME}" DISPLAY="${DISPLAY}" PULSE_RUNTIME_PATH="${PULSE_RUNTIME_PATH}" \
        /lsiopy/bin/selkies --addr=localhost --mode=websockets
else
    exec su -s /bin/bash "${USER}" -c "HOME=${HOME} DISPLAY=${DISPLAY} PULSE_RUNTIME_PATH=${PULSE_RUNTIME_PATH} \
        /lsiopy/bin/selkies --addr=localhost --mode=websockets"
fi
