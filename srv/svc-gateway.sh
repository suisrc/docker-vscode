#!/bin/bash
# ============================================================================
# svc-gateway - Kin 反向代理 (端口 7090, 无需密码)
#
# 路由：
#   /websocket   -> http://127.0.0.1:8081  (^前缀标记为服务后端, 支持WebSocket)
#   /files       -> file:///home/webtop/Desktop/
#   /            -> file:///usr/share/selkies/web/  (放最后)
# ============================================================================

export BACKEND_URL="/websocket=http://127.0.0.1:8081;/files=file:///home/webtop/Desktop/;/=file:///usr/share/selkies/web/"

# 等待 selkies 就绪 (端口 8081)
echo "等待 selkies 服务就绪..."
for i in $(seq 1 30); do
    curl -s http://127.0.0.1:8081 > /dev/null 2>&1 && break
    sleep 1
done
echo "selkies 已就绪，启动 gateway 代理..."

exec kin --backend "${BACKEND_URL}" --port ${WEBTOP_PORT:-7090}  --token "${PASSWORD:-webtop}"
