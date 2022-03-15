FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:1.65.2-debian

# https://golang.google.cn/dl/
ARG GO_VER=1.17.8
ARG GO_URL=https://dl.google.com/go/go${GO_VER}.linux-amd64.tar.gz

USER root
# install golang
RUN curl -fSL --compressed $GO_URL | tar -xz -C /usr/local && mkdir /workspace/.go

ENV GOPATH=/workspace/.go \
    GOPROXY=https://goproxy.io,direct \
    PATH=/usr/local/go/bin:/workspace/.go/bin:$PATH

# golang env
RUN go env -w GO111MODULE=on &&\
    go env -w GOPROXY=https://goproxy.io,direct

# golang extension
RUN go install github.com/mdempsky/gocode@latest &&\
    go install github.com/uudashr/gopkgs/v2/cmd/gopkgs@latest &&\
    go install github.com/ramya-rao-a/go-outline@latest &&\
    go install github.com/acroca/go-symbols@latest &&\
    go install github.com/cweill/gotests@latest &&\
    go install github.com/fatih/gomodifytags@latest &&\
    go install github.com/josharian/impl@latest &&\
    go install github.com/davidrjenni/reftools/cmd/fillstruct@latest &&\
    go install github.com/haya14busa/goplay/cmd/goplay@latest &&\
    go install github.com/godoctor/godoctor@latest &&\
    go install github.com/go-delve/delve/cmd/dlv@latest &&\
    go install github.com/stamblerre/gocode@latest &&\
    go install github.com/rogpeppe/godef@latest &&\
    go install github.com/sqs/goreturns@latest &&\
    go install golang.org/x/lint/golint@latest &&\
    go install golang.org/x/tools/cmd/goimports@latest &&\
    go install golang.org/x/tools/gopls@latest &&\
    go install golang.org/x/tools/cmd/guru@latest &&\
    go install golang.org/x/tools/cmd/gorename@latest; exit 0

RUN chown -R vscode:vscode /workspace/.go
USER vscode
# vscode extension
RUN code-server --install-extension golang.go
