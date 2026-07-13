# 镜像

## 说明

### VS Code ssh-remote 环境变量失效
问题的根源在于VSCode Remote在登录远程服务器时，使用的是Interactive login的方式，这种方式会加载/etc/profile、~/.bash_profile 、~/.bash_login /，默认并不会加载 ~/.bashrc，因此我们在bashrc中设置的环境变量也就不会在VSCode Remote中生效了。这是通常情况，有时候不是按这个顺序加载，而是直接加载~/.bashrc
所以需要把/etc/profile中的和PATH有关的内容手动放开 ~/.bashrc 来绕开这个问题。

容器中启用sshd，可以方便连接和排障，以及进行一些日常的运维操作。但是很多用户进入到容器中却发现，在docker启动时候配置的环境变量通过env命令并不能够正常显示。这个的主要原因还是ssh为用户建立连接的时候会导致环境变量被重置。这样导致的最大问题就是通过ssh启动的容器进程将无法获取到容器启动时候配置的环境变量。所以从1号进程获取容器本身的环境变量,就是export $(cat /proc/1/environ |tr '\0' '\n' | xargs)

### VS Code SSH 远程服务器
~/.bashrc 最后追加内容
export $(cat /proc/1/environ |tr '\0' '\n' | xargs)

## k8s

```sh
curl -LO https://dl.k8s.io/release/v1.xx.xx/bin/linux/amd64/kubectl
chmod +x kubectl && cp kubectl /usr/local/bin/

# 使用kubectl时候, 可以以kubectl-[command]方式定义kubectl的krew插件, 之后通过kubectl command方式调用
# kubectl-ssh: 可以管理集群中任何一个节点，而不需要密码登录, (kubectl-ssh)[https://github.com/luksa/kubectl-plugins]
# kubectl ssh node [node-name]
```

## go

```sh
go env -w GOPROXY=https://goproxy.io,direct &&
go env -w GOSUMDB="sum.golang.google.cn"
go env -w GOSUMDB="sum.golang.org"
go env -w GOSUMDB=off
```

## js

```sh
npm config set registry https://registry.npmmirror.com
```

## py

```sh
pip config set global.index-url https://mirrors.tuna.tsinghua.edu.cn/pypi/web/simple
```

## java

```sh
<?xml version="1.0" encoding="UTF-8"?>
<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0"
          xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
          xsi:schemaLocation="http://maven.apache.org/SETTINGS/1.0.0 http://maven.apache.org/xsd/settings-1.0.0.xsd">
    <!-- <localRepository>/root/.m2/repository</localRepository> -->
    <!-- <localRepository>/drone/src/repository</localRepository> -->
    <localRepository>/homg/ubuntu/.m2/repository</localRepository>
    <mirrors>
        <mirror>
            <id>maven-release</id>
            <mirrorOf>external:*,!maven-snapshots</mirrorOf>
            <url>http://nexus-ops:8081/repository/maven-rgroup/</url>
        </mirror>
        <mirror>
            <id>maven-snapshots</id>
            <mirrorOf>maven-snapshots</mirrorOf>
            <url>http://nexus-ops:8081/repository/maven-sgroup/</url>
        </mirror>
    </mirrors>
    <profiles>
        <profile>
            <id>default</id>
            <repositories>
                <repository>
                    <id>maven-snapshots</id>
                    <url>http://0.0.0.0/</url>
                    <releases> <enabled>false</enabled> </releases>
                    <snapshots> <enabled>true</enabled> </snapshots>
                </repository>
            </repositories>
            <pluginRepositories>
                <pluginRepository>
                    <id>maven-snapshots</id>
                    <url>http://0.0.0.0/</url>
                    <releases> <enabled>false</enabled> </releases>
                    <snapshots> <enabled>true</enabled> </snapshots>
                </pluginRepository>
            </pluginRepositories>
        </profile>
    </profiles>
    <activeProfiles>
        <activeProfile>default</activeProfile>
    </activeProfiles>
</settings>

```