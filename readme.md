# 说明

多环境

## Dockerfile

基础组件  
Dockerfile.ssh(proxyssh-3.0.0)(ssh代理)  
Dockerfile.ngx(ssh-3.0.0)(vsc代理)  
  
开发环境  
Dockerfile.lite(lite)(轻量版，无cjk字体)  
Dockerfile.s6   -> 只安装 s6, 并将/etc/s6-overlay/init-entry 注入到 /init 第二行  
Dockerfile.sshd -> 只启动 sshd + nginx，构建一个可以简单的用于访问的 linux 微环境  
Dockerfile.xfce -> 启动一个桌面环境，之所有桌面下放到这层，是因为桌面基本不太会进行变动  
Dockerfile.xa   -> 在xfce基础上，增加 tun2socks 和 frp 软件支持， 支持全局代理和内网穿透  
Dockerfile.vscode
Dockerfile.dev     ->  golang + nodejs + java, 由于python安装简单，不在考虑范围内 
Dockerfile.pwright ->  安装 playwirght 环境  


## 鸣谢
https://github.com/microsoft/vscode/releases  
https://github.com/gitpod-io/openvscode-server/releases  
https://github.com/coder/code-server/releases  
https://github.com/kasmtech/KasmVNC  
https://github.dev/kasmtech/workspaces-core-images  
https://github.com/linuxserver/docker-webtop  
https://github.com/linuxserver/docker-baseimage-kasmvnc  
https://github.com/linuxserver/docker-baseimage-ubuntu  

## codea


```sh
code-server --host 0.0.0.0 --port 6802 --connection-token 77885566
BACKEND_URL=http://127.0.0.1:6802 ./codea/codea

rm /var/run/vscode.sock && code-server --socket-path /var/run/vscode.sock --connection-token 77885566
BACKEND_URL=unix:///var/run/vscode.sock PROXY_USE_SSL=1 PROXY_PORT=7080 ./codea/codea

./codea/codea --ssl --backend unix:///var/run/vscode.sock --service "code-server --socket-path /var/run/vscode.sock --connection-token 77885566"

BACKEND_URL=http://127.0.0.1:6802 ./codea

curl http://127.0.0.1:7080/__vscode/api/latest/server-linux-x64-web/stable
wget --trust-server-names http://127.0.0.1:7080/__vscode/commit:7e7950df89d055b5a378379db9ee14290772148a/server-linux-x64-web/stable
```

```sh
VSCODE_CLI_UPDATE_URL=http://127.0.0.1:7080/__vscode

# **最新版本检查 API**
GET {update_endpoint}/__vscode/api/latest/{platform}/{quality}

# **下载指定 commit**
GET {update_endpoint}/__vscode/commit:{commit}/{platform}/{quality}

# **下载指定 commit**
GET {update_endpoint}/__vscode/download/{quality}/{commit}/vscode-{platform}.{ext}


# **最新版本检查 API**
# https://update.code.visualstudio.com/api/latest/server-linux-x64-web/stable
# {"name":"1.126.0","version":"7e7950df89d055b5a378379db9ee14290772148a","productVersion":"1.126.0","timestamp":1782207609516}

# **下载指定 commit** 会重定向执行下载
# https://update.code.visualstudio.com/commit:7e7950df89d055b5a378379db9ee14290772148a/server-linux-x64-web/stable
# https://vscode.download.prss.microsoft.com/dbazure/download/stable/7e7950df89d055b5a378379db9ee14290772148a/vscode-server-linux-x64-web.tar.gz

```

```sh

docker pull hkccr.ccs.tencentyun.com/suisrc/webtop:lite-3.0.1.bate2

```

### Platform Segment

| Platform | Segment |
|---|---|
| Linux x64 | `server-linux-x64-web` |
| Linux ARM64 | `server-linux-arm64-web` |
| macOS x64 | `server-darwin-web` |
| macOS ARM64 | `server-darwin-arm64-web` |
| Windows x64 | `server-win32-x64-web` |

### Quality Segment
| Quality | Segment |
|---|---|
| `Stable` | `stable` |
| `Insiders` | `insider` |
| `Exploration` | `exploration` |

## 升级说明

由于观测到vscode升级非常频繁，而且也带来了一些问题，比如插件兼容性问题。所以在20240805后的版本中，vscode, vsccdr, vscpod将只有基础版本，vsccli将提供高级功能，包括dev开发环境， xface UI页面等功能