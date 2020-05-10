# docker-python-dev

ms-python.python
```
# yum install -y make zlib-devel bzip2-devel openssl-devel ncurses-devel sqlite-devel readline-devel tk-devel gdbm-devel db4-devel libpcap-devel xz-devel
# 安装python3, 没有离线安装，make方式安装过于麻烦
yum install -y python3

ln -s /usr/bin/python3 /usr/local/bin/py
pip3 install --user pylint


pip3 install --user django
ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin

django-admin startproject demo
py manage.py migrate
py manage.py runserver 5000

${workspaceFolder}/demo/manage.py

# sqlite版本低, 升级一下
curl -L https://www.sqlite.org/2020/sqlite-autoconf-3310100.tar.gz -o sqlite-autoconf.tar.gz
tar zxvf sqlite-autoconf.tar.gz
cd  sqlite-autoconf-3310100
./configure --prefix=/usr/local
make && make install
cd .. && rm -rf sqlite-autoconf-3310100
mv /usr/bin/sqlite3  /usr/bin/sqlite3_old
ln -s /usr/local/bin/sqlite3   /usr/bin/sqlite3
echo "/usr/local/lib" > /etc/ld.so.conf.d/sqlite3.conf
ldconfig
sqlite3 -version
```