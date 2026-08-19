.PHONY: build clean

kvs: main.go login.html logout.vsc.js loading.html favicon.ico kvs.ini.example go.mod
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