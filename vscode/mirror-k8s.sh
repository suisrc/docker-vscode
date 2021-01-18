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

# Google（国外）
cat <<EOF > /etc/yum.repos.d/kubernetes.repo
[kubernetes]
name=Kubernetes
baseurl=https://packages.cloud.google.com/yum/repos/kubernetes-el7-x86_64
enabled=1
gpgcheck=0
repo_gpgcheck=0
gpgkey=https://packages.cloud.google.com/yum/doc/yum-key.gpg https://packages.cloud.google.com/yum/doc/rpm-package-key.gpg
EOF

# 普通镜像替换国内镜像
# centos
mv /etc/yum.repos.d/CentOS-Base.repo /etc/yum.repos.d/CentOS-Base.repo.bak &&\
LINUX_MIRRORS=http://mirrors.aliyun.com &&\
curl -fsSL ${LINUX_MIRRORS}/repo/Centos-8.repo -o /etc/yum.repos.d/CentOS-Base.repo &&\
sed -i -e '/mirrors.cloud.aliyuncs.com/d' -e '/mirrors.aliyuncs.com/d' /etc/yum.repos.d/CentOS-Base.repo &&\
sed -i 's/gpgcheck=1/gpgcheck=0/g' /etc/yum.repos.d/CentOS-Base.repo &&\
#curl -fsSL ${LINUX_MIRRORS}/repo/epel-7.repo -o /etc/yum.repos.d/epel.repo;
echo "[kubernetes]" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "name=Kubernetes" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "baseurl=${LINUX_MIRRORS}/kubernetes/yum/repos/kubernetes-el7-x86_64/" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "enabled=1" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "gpgcheck=0" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "repo_gpgcheck=0" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "gpgkey=${LINUX_MIRRORS}/kubernetes/yum/doc/yum-key.gpg ${LINUX_MIRRORS}/kubernetes/yum/doc/rpm-package-key.gpg" >> /etc/yum.repos.d/kubernetes.repo &&\
echo "" >> /etc/yum.repos.d/kubernetes.repo; \
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

