.PHONY: kvs build start clean help demo vscode zcodex zcode mirror proxyn deploy

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
# KVS_SVC_VSCODE_DOWNLOAD_BASE=https://oss.vsc.sims-cn.com/vsc
# KVS_SVC_VSCODE_DOWNLOAD_PATH=stable/VERHASH/vscode-server-linux-x64-web.tar.gz
vscode:
	KVS_DEBUG=1 KVS_PORT=7180 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 \
	KVS_HOME=../../temp KVS_COOKIE_TOKEN_KEY=vscode-tkn \
	go run main.go -n :vscode

zcodex:
	KVS_DEBUG=1 KVS_PORT=7188 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 \
	KVS_HOME=../../temp KVS_COOKIE_TOKEN_KEY=zcodex-tkn \
	KVS_SVC_ZCODE_VERSION=3.14.3 \
	go run main.go -n :zcodex

zcode:
	ZCODE_DESKTOP_USER_DATA_DIR=/root/.config/ZCode2 \
	ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=ws://127.0.0.1:7188/ws \
	ZCODE_WEB_REMOTE_CONTROL_URL=https://leo-7180-wsc.vsc.sims-cn.com/remote/v4 \
	DISPLAY=:1 /opt/ZCode/zcode --no-sandbox

# export KVS_MIRROR_S3_ACCESS=
# export KVS_MIRROR_S3_SECRET=
mirror:
	KVS_MIRROR_S3_PREFIX=https://oss.vsc.sims-cn.com/vsc \
	go run main.go mirror vscode -version 1.139.1

# make proxyn
proxyn:
	KVS_PORT=7180 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 KVS_LOGIN_TIMEOUT=7200 \
	go run main.go -n "/healthz=text://OK:@now;/=unix:///var/run/vscode.sock"

# replace /usr/bin/kvs
deploy:
	cp kvs /usr/bin/kvs

# update
# make clean && make build && rm -f ../../bin/kvs && cp kvs ../../bin