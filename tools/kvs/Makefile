.PHONY: build clean

kvs:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o kvs .

build: kvs

start:
	KVS_PORT=7180 KVS_LOGIN_TOKEN=123456 go run main.go -c kvs.ini

clean:
	rm -f kvs kvs.ini

help:
	go run main.go help

# make demo
demo:
	go run main.go demo

# make vscode
vscode:
	KVS_DEBUG=1 KVS_PORT=7180 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 \
	KVS_VSCODE_DOWNLOAD_BASE=https://oss.vsc.sims-cn.com/vsc \
	KVS_VSCODE_DOWNLOAD_PATH=stable/SVC_VERSION_HASH/vscode-server-linux-x64-web.tar.gz \
	KVS_HOME=../../temp/.vsc \
	go run main.go -c default

# KVS_CC_SED='src-*.js|`wss://zcode.z.ai/ws`|`wss://>host</ws`||src-*.js|`/api/v1/client/configs`,ff(e).origin|`/api/v1/client/configs`'
# KVS_CC_SED='index-*.js|overrideUrl:void 0|overrideUrl:`wss://>host</ws`'
# KVS_CC_VER_REFERER=app_version 
zcoded:
	KVS_DEBUG=1 KVS_PORT=7180 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 \
	KVS_HOME=../../temp/.zco KVS_PATH_PUBLIC='/ws|/remote/v4|/api/v1/client/configs' \
	KVS_CC_SED='src-*.js|`wss://zcode.z.ai/ws`|`wss://>host</ws`||src-*.js|`/api/v1/client/configs`,ff(e).origin|`/api/v1/client/configs`' \
	go run main.go -n "ws~/ws=wsws://zcode;cc~/api/v1/=https://zcode.z.ai/api/v1/;cc~/remote/v4=https://zcode.z.ai/remote/v4;/=api://zlist"

zcode:
	ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=ws://127.0.0.1:7180/ws \
	ZCODE_WEB_REMOTE_CONTROL_URL=https://leo-7180-wsc.kfzx.sims-cn.com/remote/v4 \
	DISPALY=:1  /opt/ZCode/zcode --no-sandbox

zcod2:
	ZCODE_HOME=/root/.zcode2 \
	ZCODE_DESKTOP_USER_DATA_DIR=/root/.config/ZCode2 \
	ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=ws://127.0.0.1:7180/ws \
	ZCODE_WEB_REMOTE_CONTROL_URL=https://leo-7180-wsc.kfzx.sims-cn.com/remote/v4 \
	DISPALY=:1  /opt/ZCode/zcode --no-sandbox
# export K3S_MIRROR_VSCODE_S3_ACCESS=
# export K3S_MIRROR_VSCODE_S3_SECRET=
mirror:
	KVS_MIRROR_VSCODE_S3_PREFIX=https://oss.vsc.sims-cn.com/vsc \
	KVS_HOME=../../temp/.vsc \
	go run main.go mirror -c default 1.129.0


# make proxyn
proxyn:
	KVS_PORT=7180 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 KVS_LOGIN_TIMEOUT=7200 \
	go run main.go -n "/healthz=text://OK:@now;/=unix:///var/run/vscode.sock"

# replace /usr/bin/kvs
deploy:
	cp kvs /usr/bin/kvs