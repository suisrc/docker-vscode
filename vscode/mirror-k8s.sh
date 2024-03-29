# 直接执行 (按照kubectl时yum依赖)

# Aliyun（国内）
cat <<EOF > /etc/yum.repos.d/kubernetes.repo
[kubernetes]
name=Kubernetes
baseurl=http://mirrors.aliyun.com/kubernetes/yum/repos/kubernetes-el7-x86_64
enabled=1
gpgcheck=0
repo_gpgcheck=0
gpgkey=http://mirrors.aliyun.com/kubernetes/yum/doc/yum-key.gpg http://mirrors.aliyun.com/kubernetes/yum/doc/rpm-package-key.gpg
EOF

cat <<EOF >/etc/apt/sources.list.d/kubernetes.list
deb http://mirrors.aliyun.com/kubernetes/apt/ kubernetes-focal main
EOF

cat <<EOF >/etc/apt/sources.list.d/docker.list
deb [arch=amd64] http://download.docker.com/linux/ubuntu $(lsb_release -cs) stable
EOF

# Google（国外）
cat <<EOF > /etc/yum.repos.d/kubernetes.repo
[kubernetes]
name=Kubernetes
baseurl=http://packages.cloud.google.com/yum/repos/kubernetes-el7-x86_64
enabled=1
gpgcheck=0
repo_gpgcheck=0
gpgkey=http://packages.cloud.google.com/yum/doc/yum-key.gpg http://packages.cloud.google.com/yum/doc/rpm-package-key.gpg
EOF

# 普通镜像替换国内镜像
# centos
mv /etc/yum.repos.d/CentOS-Base.repo /etc/yum.repos.d/CentOS-Base.repo.bak &&\
LINUX_MIRRORS=http://mirrors.aliyun.com &&\
curl -fsSL ${LINUX_MIRRORS}/repo/Centos-7.repo -o /etc/yum.repos.d/CentOS-Base.repo &&\
sed -i -e '/mirrors.cloud.aliyuncs.com/d' -e '/mirrors.aliyuncs.com/d' /etc/yum.repos.d/CentOS-Base.repo &&\
sed -i 's/gpgcheck=1/gpgcheck=0/g' /etc/yum.repos.d/CentOS-Base.repo &&\
curl -fsSL ${LINUX_MIRRORS}/repo/epel-7.repo -o /etc/yum.repos.d/epel.repo;

echo "[kubernetes]" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "name=Kubernetes" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "baseurl=${LINUX_MIRRORS}/kubernetes/yum/repos/kubernetes-el7-x86_64/" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "enabled=1" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "gpgcheck=0" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "repo_gpgcheck=0" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "gpgkey=${LINUX_MIRRORS}/kubernetes/yum/doc/yum-key.gpg ${LINUX_MIRRORS}/kubernetes/yum/doc/rpm-package-key.gpg" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "" >> /etc/yum.repos.d/kubernetes.repo;

yum install kubectl

# debian
mv /etc/apt/sources.list /etc/apt/sources.list.bak && \
LINUX_MIRRORS=http://mirrors.aliyun.com &&\
echo "deb ${LINUX_MIRRORS}/debian/ buster main non-free contrib" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/debian/ buster main non-free contrib" >>/etc/apt/sources.list &&\
echo "deb ${LINUX_MIRRORS}/debian-security buster/updates main" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/debian-security buster/updates main" >>/etc/apt/sources.list &&\
echo "deb ${LINUX_MIRRORS}/debian/ buster-updates main non-free contrib" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/debian/ buster-updates main non-free contrib" >>/etc/apt/sources.list &&\
echo "deb ${LINUX_MIRRORS}/debian/ buster-backports main non-free contrib" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/debian/ buster-backports main non-free contrib" >>/etc/apt/sources.list;

apt install kubectl

# ubuntu
mv /etc/apt/sources.list /etc/apt/sources.list.bak && \
LINUX_MIRRORS=http://mirrors.aliyun.com &&\
echo "deb ${LINUX_MIRRORS}/ubuntu/ focal main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/ubuntu/ focal main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb ${LINUX_MIRRORS}/ubuntu/ focal-security main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/ubuntu/ focal-security main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb ${LINUX_MIRRORS}/ubuntu/ focal-updates main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/ubuntu/ focal-updates main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb ${LINUX_MIRRORS}/ubuntu/ focal-proposed main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/ubuntu/ focal-proposed main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb ${LINUX_MIRRORS}/ubuntu/ focal-backports main restricted universe multiverse" >>/etc/apt/sources.list &&\
echo "deb-src ${LINUX_MIRRORS}/ubuntu/ focal-backports main restricted universe multiverse" >>/etc/apt/sources.list;

apt install kubectl

# alpine

apk add kubectl

a. /etc/apk/repositories
b. dl-cdn.alpinelinux.org => mirrors.aliyun.com
https://mirrors.aliyun.com/alpine/edge/testing

sed -i "s|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g" /etc/apk/repositories

apk add --no-cache kubectl

# k8s扩展

使用kubectl时候, 可以以kubectl-[command]方式定义kubectl的krew插件, 之后通过kubectl command方式调用

kubectl-ssh: 可以管理集群中任何一个节点，而不需要密码登录, (kubectl-ssh)[https://github.com/luksa/kubectl-plugins]
kubectl ssh node [node-name]