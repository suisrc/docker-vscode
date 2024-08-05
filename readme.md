# 说明

Dockerfile.lite(轻量版，无cjk字体，注意， 本身没有 vsode， 是 vscode-cli, 原版 )
Dockerfile.jammy(基础镜像) -> Dockerfile.kclient(修复kasm终端, 作为 xface 的补充内容)
                           -> Dockerfile.nginx -> Dockerfile.sshd -> Dockerfile.vscode |
                                                                  -> Dockerfile.vscdr  |
                                                                  -> Dockerfile.vscpod | -> Dockerfile.xface -> Dockerfile.dev -> Dockerfile.ms -> Dockerfile.playwright

## 更新

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
kclient: 修复了kasm 终端的一些问题和增加了一些功能  
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