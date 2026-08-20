#!/bin/bash
# ============================================================================
# svc-dbus - D-Bus 系统守护进程 (根据 ${USER:-} 运行时态运行)
# ============================================================================

# 运行时用户态: 默认 root, 可通过环境变量 USER 覆盖 (root 或自定义用户名)
USER="${USER:-root}"

mkdir -p /run/dbus
if [ "$USER" != "root" ]; then
    chown "${USER}:${USER}" /run/dbus
fi
rm -f /run/dbus/pid

if [ "$USER" = "root" ]; then
    exec dbus-daemon --system --nofork --nosyslog
else
    exec su -s /bin/bash "${USER}" -c "dbus-daemon --system --nofork --nosyslog"
fi
