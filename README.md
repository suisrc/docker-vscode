# 说明

提供graal, java, nodejs, golang等vscode online版本  

当前版本：1.65.2  

## 镜像：

### docker镜像：(全部)  
docker.io/suisrc/vscode  

### quay镜像：(全部)
quay.io/suisrc/vscode  

### aliyun镜像：(只提供centos版)
registry.cn-hongkong.aliyuncs.com/suisrc/vscode  

### github镜像：(该镜像只有basic版本，作为基础镜像使用)  
docker.pkg.github.com/suisrc/docker-vscode/vscode  

## TAGS

### alpine (默认使用node:14-alpine)
suisrc/vscode:[1.65.2](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-cdr](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
  
### centos (默认使用centos)
suisrc/vscode:[1.65.2-centos](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-centos-one](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-centos-onee](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-centos-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
suisrc/vscode:[1.65.2-centos-cdr](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-centos-cdr-one](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-centos-cdr-onee](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-centos-cdr-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
  
### ubuntu (默认使用ubuntu:focal)
suisrc/vscode:[1.65.2-ubuntu](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-ubuntu-one](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-ubuntu-onee](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-ubuntu-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
suisrc/vscode:[1.65.2-ubuntu-cdr](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-ubuntu-cdr-one](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-ubuntu-cdr-onee](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-ubuntu-cdr-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
  
### debian (默认使用debian:buster)
suisrc/vscode:[1.65.2-debian](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-debian-one](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-debian-onee](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-debian-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
suisrc/vscode:[1.65.2-debian-cdr](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-debian-cdr-one](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-debian-cdr-onee](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-debian-cdr-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  

### php -> 1.65.2-debian-cdr
suisrc/vscode:[1.65.2-debian-php](https://github.com/suisrc/docker-vscode/tree/dev-php)  
  
## 备注说明
1.code-server使用的版本1.60.0，不是cdr原版，为自定义版本  
2.code-server从1.65.2之后使用gitpod-io/openvscode-server，gitpod-io与microsoft更相近而且与官方迭代速度相近  
3.code-server从1.65.2之后，同步提供cdr/code-server版本，如果是docker部署，推荐使用该版本，如果是k8s部署，推荐使用gitpod版本
4.code-server提供最小版使用alpine，大小仅100MB(其中还包含插件和ZSH整体大小)  
5.删除多余版本，只保留原版， one(ts, go, ja), onee(one+插件), graal, maven, php(只保留debian)版本
  
## 历史版本

#### 1.66.1 (pod)
centos:7  
ubuntu:focal(20.04)  
debian:buster(10)  
alpine:node(14)  
[vscode](https://quay.io/repository/suisrc/vscode)

#### 1.65.2 (pod+cdr)
centos:7  
ubuntu:focal(20.04)  
debian:buster(10)  
alpine:node(14)  
[vscode](https://quay.io/repository/suisrc/vscode)

#### 1.64.2 (cdr)
centos:7  
ubuntu:focal(20.04)  
debian:buster(10)  
alpine:node(14)  
[vscode](https://quay.io/repository/suisrc/vscode)

#### 1.60.0 (cdr)
centos:7  
ubuntu:focal(20.04)  
debian:buster(10)  
[vscode](https://quay.io/repository/suisrc/vscode)

# 鸣谢
[vscode](https://github.com/microsoft/vscode/releases)  
[gitpod](https://github.com/gitpod-io/openvscode-server/releases)  
[cdr](https://github.com/cdr/code-server/releases)  