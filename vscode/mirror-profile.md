# profile 说明


## VS Code ssh-remote 环境变量失效
问题的根源在于VSCode Remote在登录远程服务器时，使用的是Interactive login的方式，这种方式会加载/etc/profile、~/.bash_profile 、~/.bash_login /，默认并不会加载 ~/.bashrc，因此我们在bashrc中设置的环境变量也就不会在VSCode Remote中生效了。这是通常情况，有时候不是按这个顺序加载，而是直接加载~/.bashrc

所以需要把/etc/profile中的和PATH有关的内容手动放~/.bashrc 来绕开这个问题。

## ssh远程docker容器环境变量缺失
容器中启用sshd，可以方便连接和排障，以及进行一些日常的运维操作。但是很多用户进入到容器中却发现，在docker启动时候配置的环境变量通过env命令并不能够正常显示。这个的主要原因还是ssh为用户建立连接的时候会导致环境变量被重置。这样导致的最大问题就是通过ssh启动的容器进程将无法获取到容器启动时候配置的环境变量。

所以从1号进程获取容器本身的环境变量,就是export $(cat /proc/1/environ |tr '\0' '\n' | xargs)

## VS Code SSH 远程服务器
~/.bashrc 最后追加内容
export $(cat /proc/1/environ |tr '\0' '\n' | xargs)