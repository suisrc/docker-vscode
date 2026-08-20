#!/usr/bin/env bash

set -e
# ============================================================
# 创建 selkies web 目录
# ============================================================
DASHBOARD="${DASHBOARD:-selkies-dashboard}"
HNAME=$(hostname)

if [ -d "/usr/share/selkies/${DASHBOARD}" ]; then
    rm -rf /usr/share/selkies/web
    cp -a "/usr/share/selkies/${DASHBOARD}" /usr/share/selkies/web
    echo "[web-selkies] selkies web 从 ${DASHBOARD} 复制完成."
else
    echo "[web-selkies] 警告: /usr/share/selkies/${DASHBOARD} 不存在，跳过 web 目录创建."
fi

# 图标
if [ -f /usr/share/selkies/www/icon.png ]; then
    cp /usr/share/selkies/www/icon.png /usr/share/selkies/web/icon.png 2>/dev/null || true
    cp /usr/share/selkies/www/icon.png /usr/share/selkies/web/favicon.ico 2>/dev/null || true
    cp /usr/share/selkies/www/logout.js /usr/share/selkies/web/logout.wtp.js 2>/dev/null || true
    cp /usr/share/selkies/web/index.html /usr/share/selkies/web/index.html.bak
    sed -i "s|</body>|<script src=\"logout.wtp.js\"></script></body>|" /usr/share/selkies/web/index.html
fi

# 动态生成 manifest.json
cat > /usr/share/selkies/web/manifest.json << MEOF
{
  "name": "${TITLE}-${HNAME}",
  "short_name": "${HNAME}",
  "manifest_version": 2,
  "version": "1.0.0",
  "display": "fullscreen",
  "background_color": "#000000",
  "theme_color": "#000000",
  "icons": [{ "src": "icon.png", "type": "image/png", "sizes": "180x180" }],
  "start_url": "/"
}
MEOF
echo "[web-selkies] manifest.json 已生成，标题: ${TITLE}-${HNAME}"

