# 组件安装在/root/.local/bin文件夹中
# pip3 install --user django
# ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin
# 
# -*- coding: UTF-8 -*-
# django框架
# /root/.local/bin/django-admin startproject demo
# cd demo
# py manage.py migrate
# sed -i "s/ALLOWED_HOSTS = \[\]/ALLOWED_HOSTS = ['*']/" demo/settings.py
# py manage.py runserver 0.0.0.0:5000
# 
# 该实例输出 Hello World!
print('Hello World!')



###################################################################
# 经测试，在python方面，graalvm支持并没有想象中友好，所以暂时不提供
# 系统使用yum按照python3， 得到python开发环境的支持
###################################################################
# 通过以下命令轻松构建python开发环境（系统中通过该方式安装）， 默认3.6
# ```
# yum install -y python3
# ln -s /usr/bin/python3 /usr/local/bin/py
# 
# pip3 install --user pylint
# pip3 install --user django
# ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin
# code-server --install-extension ms-python.python
# 
# pip3 install --upgrade pip
# ```
###################################################################
# 编译构建, 可以使用python的最新版构建
# ```
# yum install -y openssl openssl-devel libffi-devel
# curl -SL https://www.python.org/ftp/python/3.8.3/Python-3.8.3.tgz -o python-autoconf.tar.gz
# mddir python-autoconf
# tar -zxvf python-autoconf.tar.gz -C python-autoconf --strip-components=1
# cd python-autoconf
# mkdir /usr/local/python3
# ./configure --prefix=/usr/local/python3 --with-ssl
# make && make install
# ln -s /usr/local/python3/bin/python3 /usr/local/bin/python3
# ln -s /usr/local/python3/bin/pip3    /usr/local/bin/pip3
# ln -s /usr/local/python3/bin/python3 /usr/local/bin/py
# cd .. && rm -rf python-autoconf
#
# pip3 install --upgrade pip
# pip3 install --user pylint
# pip3 install --user django
# ln -s /root/.local/bin/django-admin /usr/local/bin/django-admin
# code-server --install-extension ms-python.python
# ```
###################################################################
# 当然，也可以使用graal自带python进行处理，推荐仅限测试
# 请注意：此Python实现尚处于初期阶段，目前只能运行基本基准测试。
# https://www.graalvm.org/docs/reference-manual
# ```
# gu install python
# ln -s /graalvm/bin/graalpython /usr/local/bin/py
# py -m ginstall pypi pylint
# py -m ginstall pypi django
# code-server --install-extension ms-python.python
# 
# # 安装pip3， 2种安装方式均未通过测试
# # 第一种：直接通过graalpython安装
# py -m ginstall pypi pip
# # 第二种：直接通过编译安装
# curl -SL https://files.pythonhosted.org/packages/ac/d6/0f6c0d9d0b07bbb2085e94a71aded1e137c7c9002ac54924bc1c0adf748a/setuptools-46.4.0.zip -o setuptools-46.4.0.zip
# 7za x setuptools-46.4.0.zip
# cd setuptools-46.4.0
# py setup.py build
# py setup.py install
# curl -SL https://files.pythonhosted.org/packages/08/25/f204a6138dade2f6757b4ae99bc3994aac28a5602c97ddb2a35e0e22fbc4/pip-20.1.1.tar.gz -o pip-20.1.1.tar.gz
# tar -zxvf pip-20.1.1.tar.gz
# cd pip-20.1.1
# py setup.py build
# py setup.py install
# ```
#