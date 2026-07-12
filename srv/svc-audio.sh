#!/bin/bash
# svc-audio - PulseAudio 音频守护进程
# 以 webtop 用户运行，sink 创建由 svc-selkies 负责

# 确保 webtop 用户存在
WEBTOP_UID=$(id -u webtop 2>/dev/null) || {
    echo "[svc-audio] 错误: webtop 用户不存在" >&2
    exit 1
}

PULSE_RUNTIME_PATH=/run/user/${WEBTOP_UID}/pulse
export PULSE_RUNTIME_PATH

# 确保 pulse 运行时目录存在
mkdir -p "${PULSE_RUNTIME_PATH}"

# 清理可能残留的 pulseaudio 实例（防止 "Address already in use" 和 D-Bus 冲突）
su -s /bin/bash webtop -c "PULSE_RUNTIME_PATH=${PULSE_RUNTIME_PATH} pulseaudio --kill" 2>/dev/null || true
rm -f "${PULSE_RUNTIME_PATH}/pid" 2>/dev/null
rm -f "${PULSE_RUNTIME_PATH}/native" 2>/dev/null
chown -R webtop:webtop /run/user/${WEBTOP_UID} 2>/dev/null || true

# 确保 pulse 用户配置目录存在（用于 cookie 文件）
mkdir -p /home/webtop/.config/pulse
chown -R webtop:webtop /home/webtop/.config/pulse 2>/dev/null || true

# kas 会捕获 stdout/stderr 到日志文件，不要丢弃
# device.description 属性是 WebRTC 枚举音频设备的必要条件，缺少会导致 OverconstrainedError
exec su -s /bin/bash webtop -c \
  "PULSE_RUNTIME_PATH=${PULSE_RUNTIME_PATH} /usr/bin/pulseaudio -n \
    --log-level=2 --log-target=stderr --exit-idle-time=-1 \
    -L 'module-native-protocol-unix' \
    -L 'module-null-sink sink_name=output sink_properties=device.description=output' \
    -L 'module-null-sink sink_name=input sink_properties=device.description=input'"
