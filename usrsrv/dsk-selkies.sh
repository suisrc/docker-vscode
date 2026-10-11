#!/bin/bash
# ============================================================================
# dsk-selkies - Selkies WebRTC/WebSocket 桥接服务
# 监听 localhost:8081 提供 WebSocket 接口（selkies 默认端口）
# 音频通过 PULSE_RUNTIME_PATH 连接 pulseaudio, 视频通过 X11 抓屏,
# 均不依赖 session bus (对齐上游 svc-selkies)
# ============================================================================

. /usr/srv/dsk-lib.sh
dsk_prepare_runtime

export DISPLAY=${DISPLAY:-:1}
export SELKIES_ENABLE_RESIZE=true

# 等待 X 服务就绪
for i in $(seq 1 30); do
    run_as_user xdpyinfo -display "${DISPLAY}" > /dev/null 2>&1 && break
    echo "等待 Xvfb 就绪... ($i)"
    sleep 1
done

# 设置光标主题
export XCURSOR_THEME=breeze_cursors

exec_as_user /lsiopy/bin/selkies --addr=localhost --mode=websockets
