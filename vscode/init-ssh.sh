#!/bin/sh
set -e
## !/usr/bin/execlineb -P
## chmod +x /sh/init-ssh.sh
# 安装sshd服务器(centos): yum install openssh-server -y
# 安装sshd服务器(debian): apt install openssh-server -y
# 获取远程私钥:     cat /etc/ssh/ssh_host_rsa_key
# 配置远程登录密码: echo "root:${PASSWORD}" | chpasswd
if [ -f '/etc/init.d/ssh' ]; then
  # echo "PermitRootLogin yes" >> /etc/ssh/sshd_config
  case "$1" in
    start)
      /etc/init.d/ssh start
      ;;
    stop)
      /etc/init.d/ssh stop
      ;;
    *)
      echo "Usage: {start|stop}"
      ;;
  esac
else
  if [ ! -f '/etc/ssh/ssh_host_rsa_key' ]; then
    #sshd-keygen
    ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_rsa_key
    ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_dsa_key
    ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_ecdsa_key
    ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_ed25519_key
  fi
  case "$1" in
    start)
      /usr/sbin/sshd -D &
      ;;
    stop)
      cat /var/run/sshd.pid | xargs kill -9
      ;;
    *)
      echo "Usage: {start|stop}"
      ;;
  esac
fi
