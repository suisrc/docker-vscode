#!/usr/bin/execlineb -P

if [ ! -e '/etc/init.d/ssh' ]; then
  #apt install openssh-server -y
  echo "root:${PASSWORD}" | chpasswd
  echo "PermitRootLogin yes" >> /etc/ssh/sshd_config
  
  cat > /etc/cont-init.d/ssh-init <<EOF
#!/usr/bin/execlineb -P
/etc/init.d/ssh start
EOF

  chmod +x /etc/cont-init.d/ssh-init

elif [ ! -e '/usr/sbin/sshd' ]; then
  #yum install openssh-server -y
  echo "root:${PASSWORD}" | chpasswd
  ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_rsa_key
  ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_ecdsa_key
  ssh-keygen -q -t rsa -N '' -f /etc/ssh/ssh_host_ed25519_key
  
  cat > /etc/cont-init.d/ssh-init <<EOF
#!/usr/bin/execlineb -P
/usr/sbin/sshd -D
EOF

  chmod +x /etc/cont-init.d/ssh-init

if