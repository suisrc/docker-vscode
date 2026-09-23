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

zcodex:
	KVS_DEBUG=1 KVS_PORT=7188 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 \
	KVS_HOME=../../temp/.vsc \
	go run main.go -c zcodex
	
zcode:
	ZCODE_DESKTOP_USER_DATA_DIR=/root/.config/ZCode2 \
	ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=ws://127.0.0.1:7188/ws \
	ZCODE_WEB_REMOTE_CONTROL_URL=https://leo-7180-wsc.vsc.sims-cn.com/remote/v4 \
	DISPLAY=:1 /opt/ZCode/zcode --no-sandbox

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

# update
# make clean && make build && rm -f ../../bin/kvs && cp kvs ../../bin