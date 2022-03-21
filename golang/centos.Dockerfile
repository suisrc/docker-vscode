FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.65.2.2-centos

# https://golang.google.cn/dl/
ARG GO_VER=1.17.8
ARG GO_URL=https://dl.google.com/go/go${GO_VER}.linux-amd64.tar.gz

USER root
# install golang
RUN curl -fSL --compressed $GO_URL | tar -xz -C /usr/local

ENV PATH=/usr/local/go/bin:/workspace/.go/bin:$PATH \
    GOPATH=/workspace/.go

USER vscode
# golang extension
RUN mkdir /workspace/.go &&\
    go install github.com/ramya-rao-a/go-outline@latest &&\
    go install github.com/cweill/gotests/gotests@latest &&\
    go install github.com/fatih/gomodifytags@latest &&\
    go install github.com/josharian/impl@latest &&\
    go install github.com/haya14busa/goplay/cmd/goplay@latest &&\
    go install github.com/go-delve/delve/cmd/dlv@latest &&\
    go install honnef.co/go/tools/cmd/staticcheck@latest &&\
    go install golang.org/x/tools/gopls@latest; exit 0

# golang env
#RUN go env -w GO111MODULE=on &&\
#    go env -w GOPROXY=https://goproxy.io,direct

# vscode extension
RUN code-server --install-extension golang.go
