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

# KVS_CC_SED='src-*.js|`wss://zcode.z.ai/ws`|`wss://>host</ws`'
# KVS_CC_SED='index-*.js|overrideUrl:void 0|overrideUrl:`wss://>host</ws`'
# ws~/ws=http://127.0.0.1:8081;
zcode:
	KVS_DEBUG=1 KVS_PORT=7180 KVS_LOGIN_AUTHZ=true KVS_LOGIN_TOKEN=123456 \
	KVS_HOME=../../temp/.zco KVS_CC_VER_REFERER=app_version \
	KVS_CC_SED='index-*.js|overrideUrl:void 0|overrideUrl:`wss://>host</ws`' \
	go run main.go -n "ws~/ws=http://127.0.0.1:8787;cc~/remote/v4=https://zcode.z.ai/remote/v4"

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