# 说明

多环境

## Dockerfile

基础组件  
Dockerfile.ssh(proxyssh-3.0.0)(ssh代理)  
Dockerfile.ngx(ssh-3.0.0)(vsc代理)  
  
开发环境  
Dockerfile.vsc -> vscode online 
Dockerfile.xfc -> 启动一个桌面环境，之所有桌面下放到这层，是因为桌面基本不太会进行变动  
Dockerfile.dev-vsc ->  nvm(nodejs) + nv(python) + sdk(java) + golang  
Dockerfile.dev-xfc ->  golang + nodejs + java, 由于python安装简单，不在考虑范围内  
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

## Code Image

```sh

docker pull ghcr.io/suisrc/webtop:vsc-3.0.0
docker pull docker.io/suisrc/webtop:vsc-3.0.0
docker pull hkccr.ccs.tencentyun.com/suisrc/webtop:vsc-3.0.0

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
