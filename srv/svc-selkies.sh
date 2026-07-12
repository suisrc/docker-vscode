#!/bin/bash
# ============================================================================
# svc-selkies - Selkies WebRTC/WebSocket 桥接服务
# 监听 localhost:8081 提供 WebSocket 接口（selkies 默认端口）
# ============================================================================

export HOME=${HOME:-/home/webtop}
export DISPLAY=${DISPLAY:-:1}
export SELKIES_ENABLE_RESIZE=true

# 等待 X 服务就绪
for i in $(seq 1 30); do
    su -s /bin/bash webtop -c "DISPLAY=${DISPLAY} xdpyinfo -display ${DISPLAY} > /dev/null 2>&1" && break
    echo "等待 Xvfb 就绪... ($i)"
    sleep 1
done

# 设置光标主题
export XCURSOR_THEME=breeze_cursors

# PulseAudio 音频 (selkies 需要 PULSE_RUNTIME_PATH 才能连接 pulseaudio)
export PULSE_RUNTIME_PATH=/run/user/$(id -u webtop)/pulse

exec su -s /bin/bash webtop -c "HOME=${HOME} DISPLAY=${DISPLAY} PULSE_RUNTIME_PATH=${PULSE_RUNTIME_PATH} \
    /lsiopy/bin/selkies --addr=localhost --mode=websockets"
