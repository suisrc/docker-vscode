# 说明

## 说明

FRP_TOKEN : FRP认证，没有默认值  
FRP_ADDR ：FRP服务地址，没有默认值，如果存在，即标识需要连接服务  
FRP_PORT ：FRP服务端口，默认值为 9000  
FRP_USER ：FRP唯一标识，默认值为 cat /etc/hostname  
FRP_FILE : FRP配置文件，默认值为 /etc/frp/frpc.toml  


## frpc.toml

```sh http subdomain => hostname
# server address
FRP_ADDR=192.168.111.222
# server port
FRP_PORT=9000
# server token
FRP_TOKEN=xxxxxx
# server name
FRP_USER=hostname

# code online => vsc-hostname.xxx.local
# desk online => dsk-hostname.xxx.local
```

## frp1.toml

```sh https custom domains
# server address
FRP_ADDR=192.168.111.222
# server port
FRP_PORT=9000
# server token
FRP_TOKEN=xxxxxx
# server name
FRP_USER=hostname
# custom domain name => vsc.xxx.local
FRP_DN_VSC=vsc.xxx.local
# custom domain name => dsk.xxx.local
FRP_DN_DSK=dsk.xxx.local

```

## frp2.toml

```sh http subdomain
# server address
FRP_ADDR=192.168.111.222
# server port
FRP_PORT=9000
# server token
FRP_TOKEN=xxxxxx
# server name
FRP_USER=hostname
# use frp local doman => xxx.local

# code online => vsc.xxx.local
FRP_DS_VSC=vsc
# desk online => dsk.xxx.local
FRP_DS_DSK=dsk
```

## frp3.toml

```sh http custom domains
# server address
FRP_ADDR=192.168.111.222
# server port
FRP_PORT=9000
# server token
FRP_TOKEN=xxxxxx
# server name
FRP_USER=hostname
# custom domain name => vsc.xxx.local
FRP_DN_VSC=vsc.xxx.local
# custom domain name => dsk.xxx.local
FRP_DN_DSK=dsk.xxx.local
```