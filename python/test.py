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
