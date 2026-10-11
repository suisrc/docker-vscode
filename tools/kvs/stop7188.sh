#!/bin/bash

## 关闭占用指定 TCP 端口(默认 7188)的监听进程
#
# 用法:
#   bash stop7188.sh            # 关闭 7188 端口占用进程
#   bash stop7188.sh 7180       # 指定其它端口
#   PORT=7180 bash stop7188.sh  # 通过环境变量指定端口
#
# 说明:
#   make zcodex 会以 KVS_PORT=7188 运行 `go run main.go -n :zcodex`，
#   退出时残留的临时进程常驻监听 7188，本脚本用于清理它（先 TERM 再 KILL）。
#
# 端口来源优先级: 命令行参数 > 环境变量 PORT > 默认 7188

set -eo pipefail

PORT="${1:-${PORT:-7188}}"

## ---------- 收集监听该端口的 PID ----------
# 依次尝试 ss / lsof / fuser，取第一个有结果的来源
list_pids() {
    local pids=""

    if command -v ss >/dev/null 2>&1; then
        pids=$(ss -ltnpH "sport = :$PORT" 2>/dev/null \
            | grep -oE 'pid=[0-9]+' | cut -d= -f2)
    fi

    if [[ -z "$pids" ]] && command -v lsof >/dev/null 2>&1; then
        pids=$(lsof -nP -tiTCP:"$PORT" -sTCP:LISTEN 2>/dev/null)
    fi

    if [[ -z "$pids" ]] && command -v fuser >/dev/null 2>&1; then
        pids=$(fuser "$PORT/tcp" 2>/dev/null | tr -s ' ' '\n')
    fi

    # 去空行、去重、剔除自身 PID
    echo "$pids" | tr -s ' \t' '\n' | grep -E '^[0-9]+$' | grep -v "^$$$" | sort -u
}

PIDS=$(list_pids || true)

if [[ -z "$PIDS" ]]; then
    echo "端口 $PORT 未被占用"
    exit 0
fi

echo "端口 $PORT 占用进程:"
for pid in $PIDS; do
    echo "  pid=$pid  $(ps -p "$pid" -o user=,args= 2>/dev/null || echo '(已退出)')"
done

## ---------- 优雅终止，失败则强制 ----------
for pid in $PIDS; do
    kill "$pid" 2>/dev/null || true
done

for _ in 1 2 3 4 5; do
    sleep 1
    alive=""
    for pid in $PIDS; do
        kill -0 "$pid" 2>/dev/null && alive="$alive $pid"
    done
    [[ -z "$alive" ]] && break
done

# 仍存活则 -9
for pid in $PIDS; do
    if kill -0 "$pid" 2>/dev/null; then
        echo "强制结束 pid=$pid"
        kill -9 "$pid" 2>/dev/null || true
    fi
done

sleep 1

## ---------- 复查 ----------
if [[ -n "$(list_pids || true)" ]]; then
    echo "警告: 端口 $PORT 仍被占用" >&2
    exit 1
fi

echo "端口 $PORT 已释放"
