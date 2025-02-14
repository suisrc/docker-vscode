# 说明

使用微软官方的 vscode

## 构建

这是一种通过 code-cli 获取WebUI的方式

```sh
# vscli_tmp 工作目录
# data-cli  应用目录
# data-vsc  数据目录
mkdir vscli_svc data-cli data-vsc
# 获取控制命令
curl -Lk 'https://code.visualstudio.com/sha/download?build=stable&os=cli-alpine-x64' --output vsc.tar.gz
tar -xf vsc.tar.gz && mv code code-cli
# 启动开发环境
./code-cli serve-web --accept-server-license-terms --host 127.0.0.1 --port 8000 --cli-data-dir ./data-cli --server-data-dir ./data-vsc --without-connection-token
# ... 等待启动完成
# 复制数据, 其中 vscli_svc/ 就是我们需要的数据
rm -rf vscli_svc/* && cp -r data-cli/serve-web/fabdb6a30b49f79a7aba0f2ad9df9b399473380f/* vscli_svc/
rm -f  vscli_svc/node
# 构建
tar -cvf vscli_svc.tar -C vscli_svc .
# 测试
tar -xvf vscli_svc.tar -C vscli_tmp

# 安装gh
sudo apt-key adv --keyserver keyserver.ubuntu.com --recv-key C99B11DEB97541F0
sudo apt-add-repository https://cli.github.com/packages
sudo apt update
sudo apt install gh
# 上传 github
gh release create v1.96.2
# 获取上一个版本的hash值
gh release view v1.96.2 | grep 'Parent' | awk '{print $2}'
# 上传文件
# gh release upload v1.92.0 file.txt --clobber --target <SHA_of_previous_version>
# vscode-v1.92.0-linux-x64.tar
gh release upload v1.96.2 vscli_svc.tar
```