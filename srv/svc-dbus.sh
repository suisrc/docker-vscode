#!/bin/bash
# ============================================================================
# svc-dbus - D-Bus 系统守护进程 (以 webtop 用户运行)
# ============================================================================

mkdir -p /run/dbus
chown webtop:webtop /run/dbus
rm -f /run/dbus/pid

exec su -s /bin/bash webtop -c "dbus-daemon --system --nofork --nosyslog"
