#!/bin/bash
# ============================================================================
# dsk-audio - PulseAudio 音频守护进程
# null sink (output/input) 随守护进程加载; device.description 属性是
# WebRTC 枚举音频设备的必要条件, 缺少会导致 OverconstrainedError
# ============================================================================

. /usr/srv/dsk-lib.sh

# 确保目标用户存在
if [ "${DSK_USER}" != "root" ] && ! id "${DSK_USER}" >/dev/null 2>&1; then
    echo "[dsk-audio] 错误: ${DSK_USER} 用户不存在" >&2
    exit 1
fi

dsk_prepare_runtime

# 清理可能残留的 pulseaudio 实例（防止 "Address already in use" 和 D-Bus 冲突）
run_as_user pulseaudio --kill 2>/dev/null || true
rm -f "${PULSE_RUNTIME_PATH}/pid" "${PULSE_RUNTIME_PATH}/native"

# pulse 运行时目录与用户 cookie 目录
mkdir -p "${PULSE_RUNTIME_PATH}" "${DSK_HOME}/.config/pulse"
if [ "${DSK_USER}" != "root" ]; then
    chown -R "${DSK_USER}:" "${DSK_RUNTIME_DIR}" "${DSK_HOME}/.config/pulse" 2>/dev/null || true
fi

# kas 会捕获 stdout/stderr 到日志文件，不要丢弃
PULSE_ARGS=(
    -n
    --log-level=2
    --log-target=stderr
    --exit-idle-time=-1
    -L module-native-protocol-unix
    -L 'module-null-sink sink_name=output sink_properties=device.description=output'
    -L 'module-null-sink sink_name=input sink_properties=device.description=input'
)

exec_as_user /usr/bin/pulseaudio "${PULSE_ARGS[@]}"
