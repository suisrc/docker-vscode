#!/bin/bash
# svc-audio - PulseAudio 音频守护进程
# 以 webtop 用户运行，sink 创建由 svc-selkies 负责

PULSE_RUNTIME_PATH=/run/user/$(id -u webtop)/pulse
export PULSE_RUNTIME_PATH

# 确保 pulse 运行时目录存在
rm -f "${PULSE_RUNTIME_PATH}/pid" 2>/dev/null
mkdir -p "${PULSE_RUNTIME_PATH}"
chown -R webtop:webtop /run/user/$(id -u webtop) 2>/dev/null || true

# kas 会捕获 stdout/stderr 到日志文件，不要丢弃
# device.description 属性是 WebRTC 枚举音频设备的必要条件，缺少会导致 OverconstrainedError
exec su -s /bin/bash webtop -c \
  "PULSE_RUNTIME_PATH=${PULSE_RUNTIME_PATH} /usr/bin/pulseaudio -n \
    --log-level=2 --log-target=stderr --exit-idle-time=-1 \
    -L 'module-native-protocol-unix' \
    -L 'module-null-sink sink_name=output sink_properties=device.description=output' \
    -L 'module-null-sink sink_name=input sink_properties=device.description=input' \
    -L 'module-always-sink sink_name=output' \
    -L 'module-remap-source source_name=record master=output.monitor source_properties=device.description=record'"
