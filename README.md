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
  
### centos (默认使用centos)
suisrc/vscode:[1.65.2-centos](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-centos-nodejs](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-centos-golang](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-centos-java](https://github.com/suisrc/docker-vscode/tree/dev-java)  
suisrc/vscode:[1.65.2-centos-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
suisrc/vscode:[1.65.2-centos-python](https://github.com/suisrc/docker-vscode/tree/dev-python)  
  
### ubuntu (默认使用ubuntu:focal)
suisrc/vscode:[1.65.2-ubuntu](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
  
### debian (默认使用debian:buster)
suisrc/vscode:[1.65.2-debian](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.65.2-debian-nodejs](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.65.2-debian-golang](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.65.2-debian-java](https://github.com/suisrc/docker-vscode/tree/dev-java)  
suisrc/vscode:[1.65.2-debian-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
suisrc/vscode:[1.65.2-debian-python](https://github.com/suisrc/docker-vscode/tree/dev-python)  
suisrc/vscode:[1.65.2-debian-php](https://github.com/suisrc/docker-vscode/tree/dev-php)  
  
## 备注说明
1.鉴于centos8于2021年12月结束支持， 从1.52.1后还原到centos7， 1.47.3将会是最后一个centos8版本  
2.code-server从3.8后使用cdr/code-server原版, 不在使用再次封装版本  
3.code-server使用的cdr最后一个版本1.60.0，并发cdr原版，为自定义版  
4.code-server从1.65.2之后使用gitpod-io/openvscode-server，gitpod-io与microsoft更相近而且与官方迭代速度相近  
5.code-server提供最小版使用alpine，大小仅100MB(其中还包含插件和ZSH整体大小)  
  
## 历史版本

#### 1.60.0 centos:7
[1.57.1-centos](https://quay.io/repository/suisrc/vscode)

#### 1.60.0 ubuntu:focal(20.04)
[1.57.1-ubuntu](https://quay.io/repository/suisrc/vscode)

#### 1.60.0 debian:buster(10)
[1.57.1-debian](https://quay.io/repository/suisrc/vscode)

#### 1.57.1 ubuntu:focal
[1.57.1](https://quay.io/repository/suisrc/vscode)

#### 1.57.1 centos:7
[1.57.1-centos](https://quay.io/repository/suisrc/vscode)

#### 1.57.1 ubuntu:20.04
[1.57.1-ubuntu](https://quay.io/repository/suisrc/vscode)

#### 1.57.1 debian:buster(10)
[1.57.1-debian](https://quay.io/repository/suisrc/vscode)

#### 1.54.2 ubuntu:focal
[1.54.2](https://quay.io/repository/suisrc/vscode)

#### 1.54.2 centos:7
[1.54.2-centos](https://quay.io/repository/suisrc/vscode)

#### 1.54.2 ubuntu:20.04
[1.54.2-ubuntu](https://quay.io/repository/suisrc/vscode)

#### 1.54.2 debian:buster(10)
[1.54.2-debian](https://quay.io/repository/suisrc/vscode)

#### 1.53.2 centos:7
[1.53.2-centos](https://quay.io/repository/suisrc/vscode)

#### 1.53.2 debian:buster(10)
[1.53.2](https://quay.io/repository/suisrc/vscode)
[1.53.2-debian](https://quay.io/repository/suisrc/vscode)

#### 1.52.1 centos:7
[1.52.1-centos](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.52.1 debian:buster(10)
[1.52.1](https://hub.docker.com/r/suisrc/vscode/tags)
[1.52.1-debian](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.47.3 centos:8
[1.47.3-centos](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.47.3 debian:buster(10)
[1.47.3](https://hub.docker.com/r/suisrc/vscode/tags)
[1.47.3-debian](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.47.2 centos:8
[1.47.2-centos](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.47.2 debian:buster(10)
[1.47.2](https://hub.docker.com/r/suisrc/vscode/tags)
[1.47.2-debian](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.45.1 centos:8
[1.45.1-01-centos](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-centos-nodej](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-centos-golang](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-centos-java](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-centos-graal](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-centos-python](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.45.1 debian:buster(10)
[1.45.1-01](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-debian](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-debian-nodej](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-debian-golang](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-debian-java](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-debian-graal](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-debian-python](https://hub.docker.com/r/suisrc/vscode/tags)
[1.45.1-01-debian-php](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.44.2 centos:7
[1.44.2-01-centos-nodej](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-centos-golang](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-centos-java](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-centos-graal](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-centos-python](https://hub.docker.com/r/suisrc/vscode/tags)

#### 1.44.2 debian:buster(10)
[1.44.2-01-debian-nodej](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-debian-golang](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-debian-java](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-debian-graal](https://hub.docker.com/r/suisrc/vscode/tags)
[1.44.2-01-debian-python](https://hub.docker.com/r/suisrc/vscode/tags)

# 鸣谢
[vscode](https://github.com/microsoft/vscode/releases)  
[gitpod](https://github.com/gitpod-io/openvscode-server/releases)  
[cdr](https://github.com/cdr/code-server/releases)  