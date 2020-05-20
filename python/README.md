# docker-python-dev

ms-python.python
```
# yum install -y make zlib-devel bzip2-devel openssl-devel ncurses-devel sqlite-devel readline-devel tk-devel gdbm-devel db4-devel libpcap-devel xz-devel
# 安装python3, 没有离线安装，make方式安装过于麻烦
yum install -y python3
apt install -y python3 python3-pip

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


curl -SL https://files.pythonhosted.org/packages/ac/d6/0f6c0d9d0b07bbb2085e94a71aded1e137c7c9002ac54924bc1c0adf748a/setuptools-46.4.0.zip -o setuptools-46.4.0.zip
7za x setuptools-46.4.0.zip
cd setuptools-46.4.0
py setup.py build
py setup.py install

curl -SL https://files.pythonhosted.org/packages/08/25/f204a6138dade2f6757b4ae99bc3994aac28a5602c97ddb2a35e0e22fbc4/pip-20.1.1.tar.gz -o pip-20.1.1.tar.gz
tar -zxvf pip-20.1.1.tar.gz
cd pip-20.1.1
py setup.py build
py setup.py install