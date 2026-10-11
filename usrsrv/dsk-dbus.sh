#!/bin/bash
# ============================================================================
# dsk-dbus - D-Bus system bus (全容器唯一, 与登录用户 USER 无关)
#
# system bus 必须以 root 启动: dbus-daemon 依据 /usr/share/dbus-1/system.conf
# 的 <user>messagebus</user> 自动降权到 messagebus 用户运行。
# 不能以普通用户启动 system bus —— 服务激活(需 setuid 到各服务用户)、
# pid 文件、at_console 策略全部失效 (对齐上游 svc-dbus)。
#
# session bus 由 dsk-desktop 用 dbus-run-session 创建 (对齐上游 startwm.sh
# 的 dbus-launch --exit-with-session), selkies/pulseaudio 不需要 session bus。
# ============================================================================

mkdir -p /run/dbus
chown messagebus:messagebus /run/dbus
rm -f /run/dbus/pid

exec dbus-daemon --system --nofork --nosyslog
