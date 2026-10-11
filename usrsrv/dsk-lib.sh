#!/bin/bash
# ============================================================================
# dsk-lib - dsk-* 服务脚本共享库 (由各 dsk-* 脚本 source, 不直接执行)
#
# 约定:
#   - 容器以 root 运行, 实际用户由环境变量 USER 决定 (root 或普通用户)
#   - DISPLAY/XDG_RUNTIME_DIR/PULSE_RUNTIME_PATH 等环境直接透传给目标用户;
#     HOME 由 exec_as_user/run_as_user 显式传递 (runuser 默认会按 passwd
#     条目重置 HOME/USER/SHELL, 显式传递保证与 dsk-* 建目录的路径一致)
#   - 长驻服务用 exec_as_user 作为脚本最后一条语句 (保持 exec 信号语义);
#     探测/一次性命令用 run_as_user (前台子进程)
# ============================================================================

DSK_USER="${USER:-root}"
if [ "${DSK_USER}" = "root" ]; then
    DSK_HOME="/root"
    DSK_UID=0
else
    DSK_HOME="/home/${DSK_USER}"
    DSK_UID="$(id -u "${DSK_USER}" 2>/dev/null)" || DSK_UID=0
fi

# 标准运行时目录: XDG_RUNTIME_DIR (pulse/wayland/at-spi 等客户端约定路径)
DSK_RUNTIME_DIR="/run/user/${DSK_UID}"

# 准备运行时目录并导出标准环境 (长驻服务启动前调用一次)
dsk_prepare_runtime() {
    mkdir -p "${DSK_RUNTIME_DIR}"
    chmod 700 "${DSK_RUNTIME_DIR}" 2>/dev/null || true
    if [ "${DSK_USER}" != "root" ]; then
        chown "${DSK_USER}:" "${DSK_RUNTIME_DIR}" 2>/dev/null || true
    fi
    export XDG_RUNTIME_DIR="${DSK_RUNTIME_DIR}"
    export PULSE_RUNTIME_PATH="${DSK_RUNTIME_DIR}/pulse"
    export HOME="${DSK_HOME}"
}

# 以目标用户身份执行命令 (前台子进程, 适合探测与一次性命令)
run_as_user() {
    if [ "$(id -u)" = "0" ] && [ "${DSK_USER}" != "root" ]; then
        runuser -u "${DSK_USER}" -- env HOME="${DSK_HOME}" "$@"
    else
        "$@"
    fi
}

# 以目标用户身份 exec (仅作为脚本最后一条语句使用, 保持信号语义)
exec_as_user() {
    if [ "$(id -u)" = "0" ] && [ "${DSK_USER}" != "root" ]; then
        exec runuser -u "${DSK_USER}" -- env HOME="${DSK_HOME}" "$@"
    else
        exec "$@"
    fi
}
