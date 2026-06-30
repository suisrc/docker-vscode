# 说明

多环境

## Dockerfile

基础组件  
Dockerfile.ssh(proxyssh-3.0.0)(ssh代理)  
Dockerfile.ngx(ssh-3.0.0)(vsc代理)  
  
开发环境  
Dockerfile.lite -> vscode online 
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

## Code Image

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