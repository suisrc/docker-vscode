# 说明

提供graal, java, nodejs, golang等vscode online版本  

当前版本：1.53.2  

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

### centos (默认使用centos)
suisrc/vscode:[1.53.2-centos](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.53.2-centos-nodejs](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.53.2-centos-golang](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.53.2-centos-java](https://github.com/suisrc/docker-vscode/tree/dev-java)  
suisrc/vscode:[1.53.2-centos-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
suisrc/vscode:[1.53.2-centos-python](https://github.com/suisrc/docker-vscode/tree/dev-python)  
suisrc/vscode:[1.53.2-centos-one](https://github.com/suisrc/docker-vscode/tree/dev-one)  
  

### debian (默认使用debian:buster)
suisrc/vscode:[1.53.2](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.53.2-debian](https://github.com/suisrc/docker-vscode/tree/dev-vscode)  
suisrc/vscode:[1.53.2-debian-nodejs](https://github.com/suisrc/docker-vscode/tree/dev-nodejs)  
suisrc/vscode:[1.53.2-debian-golang](https://github.com/suisrc/docker-vscode/tree/dev-golang)  
suisrc/vscode:[1.53.2-debian-java](https://github.com/suisrc/docker-vscode/tree/dev-java)  
suisrc/vscode:[1.53.2-debian-graal](https://github.com/suisrc/docker-vscode/tree/dev-graal)  
suisrc/vscode:[1.53.2-debian-python](https://github.com/suisrc/docker-vscode/tree/dev-python)  
suisrc/vscode:[1.53.2-debian-php](https://github.com/suisrc/docker-vscode/tree/dev-php)  
  
## 备注说明
1.鉴于centos8于2021年12月结束支持， 从1.52.1后的centos版本将会还原到centos7， 1.47.3将会是最后一个centos8版本  
2.code-server使用cdr/code-server(3.8), 不在使用再次封装版本  
  
## 历史版本

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
[code-server](https://github.com/cdr/code-server/releases)  