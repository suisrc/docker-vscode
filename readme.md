# 说明

多环境

## Dockerfile

基础组件  
Dockerfile.ssh(proxyssh-2.0.0)(ssh代理)  
Dockerfile.ngx(ssh-2.2.2)(vsc代理)  
  
开发环境  
Dockerfile.lite(lite)(轻量版，无cjk字体，注意， 无 vsode， 是 vscode-cli)  
Dockerfile.s6   -> 只安装 s6, 并将/etc/s6-overlay/init-entry 注入到 /init 第二行  
Dockerfile.sshd -> 只启动 sshd + nginx，构建一个可以简单的用于访问的 linux 微环境  
Dockerfile.xfce -> 启动一个桌面环境，之所有桌面下放到这层，是因为桌面基本不太会进行变动  
Dockerfile.xa   -> 在xfce基础上，增加 tun2socks 和 frp 软件支持， 支持全局代理和内网穿透  
  
提供以下四个版本，可建立在 nginx 或者 xfce 基础是
Dockerfile.vscode(vscode版) |  
Dockerfile.vscpod(gitpod版) |  
Dockerfile.vsccdr(coder版) ->  
  
Dockerfile.dev ->  golang + nodejs + java, 由于python安装简单，不在考虑范围内  
Dockerfile.ms ->   安装 vscode 插件，并切换为 ms 源  
Dockerfile.pw ->   安装 playwirght 环境  


## 鸣谢
https://github.com/microsoft/vscode/releases  
https://github.com/gitpod-io/openvscode-server/releases  
https://github.com/coder/code-server/releases  
https://github.com/kasmtech/KasmVNC  
https://github.dev/kasmtech/workspaces-core-images  
https://github.com/linuxserver/docker-webtop  
https://github.com/linuxserver/docker-baseimage-kasmvnc  
https://github.com/linuxserver/docker-baseimage-ubuntu  

## 其他
lite: vscode 轻量版本，cjk 字体不存在  
jammy: ubuntu 22.04基础镜像  
nginx: jammy基础上安装了nginx  
sshd: nginx基础上安装了sshd  
nodejs: nodejs的环境  
vscode: vsccdr, vscpod; sshd基础上安装了vscode  
  
xface-~: 安装桌面环境  
dev-~: 专注于开发环境  
ms-dev-~: 开发环境，使用 ms 插件库  
playwright-~: 开发环境，使用 playwright-ms 插件库  

## 升级说明

由于观测到vscode升级非常频繁，而且也带来了一些问题，比如插件兼容性问题。所以在20240805后的版本中，vscode, vsccdr, vscpod将只有基础版本，vsccli将提供高级功能，包括dev开发环境， xface UI页面等功能