# 说明


## frpc.toml

```sh http subdomain => hostname
# server address， 没有默认值，如果存在，即标识需要连接服务 
FRP_ADDR=192.168.111.222
# server port
FRP_PORT=9000
# server token RP认证，没有默认值
FRP_TOKEN=
# server name
FRP_USER=`cat hostname`
# use frp local doman => xxx.local
# code online => vsc-hostname.xxx.local
# desk online => dsk-hostname.xxx.local
# 配置文件路径
FRP_FILE=/etc/frp/frpc.toml 
```
