#!/bin/bash
# svc-audio - PulseAudio 音频守护进程
# 根据 ${USER:-} 运行时态运行，sink 创建由 svc-selkies 负责

# 运行时用户态: 默认 root, 可通过环境变量 USER 覆盖 (root 或自定义用户名)
USER="${USER:-root}"
if [ "$USER" = "root" ]; then
    HOME_DIR="/root"
else
    HOME_DIR="/home/${USER}"
fi

# 确保目标用户存在
USER_UID=$(id -u "${USER}" 2>/dev/null) || {
    echo "[svc-audio] 错误: ${USER} 用户不存在" >&2
    exit 1
}

PULSE_RUNTIME_PATH=/run/user/${USER_UID}/pulse
export PULSE_RUNTIME_PATH

# 确保 pulse 运行时目录存在
mkdir -p "${PULSE_RUNTIME_PATH}"

# 清理可能残留的 pulseaudio 实例（防止 "Address already in use" 和 D-Bus 冲突）
if [ "$USER" = "root" ]; then
    PULSE_RUNTIME_PATH="${PULSE_RUNTIME_PATH}" pulseaudio --kill 2>/dev/null || true
else
    su -s /bin/bash "${USER}" -c "PULSE_RUNTIME_PATH=${PULSE_RUNTIME_PATH} pulseaudio --kill" 2>/dev/null || true
fi
rm -f "${PULSE_RUNTIME_PATH}/pid" 2>/dev/null
rm -f "${PULSE_RUNTIME_PATH}/native" 2>/dev/null

if [ "$USER" != "root" ]; then
    chown -R "${USER}:${USER}" /run/user/${USER_UID} 2>/dev/null || true
fi

# 确保 pulse 用户配置目录存在（用于 cookie 文件）
mkdir -p "${HOME_DIR}/.config/pulse"
if [ "$USER" != "root" ]; then
    chown -R "${USER}:${USER}" "${HOME_DIR}/.config/pulse" 2>/dev/null || true
fi

# kas 会捕获 stdout/stderr 到日志文件，不要丢弃
# device.description 属性是 WebRTC 枚举音频设备的必要条件，缺少会导致 OverconstrainedError
PULSE_CMD="/usr/bin/pulseaudio -n \
    --log-level=2 --log-target=stderr --exit-idle-time=-1 \
    -L 'module-native-protocol-unix' \
    -L 'module-null-sink sink_name=output sink_properties=device.description=output' \
    -L 'module-null-sink sink_name=input sink_properties=device.description=input'"

if [ "$USER" = "root" ]; then
    exec env PULSE_RUNTIME_PATH="${PULSE_RUNTIME_PATH}" ${PULSE_CMD}
else
    exec su -s /bin/bash "${USER}" -c "PULSE_RUNTIME_PATH=${PULSE_RUNTIME_PATH} ${PULSE_CMD}"
fi
