#FROM suisrc/vscode:1.44.2-01-centos
FROM docker.pkg.github.com/suisrc/docker-vscode/vscode:centos

ARG GO_VER=1.14.2
ARG GO_URL=https://dl.google.com/go/go${GO_VER}.linux-amd64.tar.gz

RUN apt-get update && apt-get install -y gcc libz-dev && \
    apt-get autoremove -y && apt-get clean && rm -rf /tmp/* /var/tmp/* /var/lib/apt/lists/*

# install golang
RUN curl -fSL --compressed  $GO_URL | tar -xz -C /usr/local

ENV $PATH=/usr/local/go/bin:$PATH
ENV GOPATH=/root/go
ENV GOPROXY=

# golang env
RUN go env -w GO111MODULE=on &&\
    go env -w GOPROXY=https://goproxy.io,direct

# golang extension
RUN go get -u github.com/mdempsky/gocode &&\
    go get -u github.com/uudashr/gopkgs/v2/cmd/gopkgs &&\
    go get -u github.com/ramya-rao-a/go-outline &&\
    go get -u github.com/acroca/go-symbols &&\
    go get -u github.com/cweill/gotests &&\
    go get -u github.com/fatih/gomodifytags &&\
    go get -u github.com/josharian/impl &&\
    go get -u github.com/davidrjenni/reftools/cmd/fillstruct &&\
    go get -u github.com/haya14busa/goplay/cmd/goplay &&\
    go get -u github.com/godoctor/godoctor &&\
    go get -u github.com/go-delve/delve/cmd/dlv &&\
    go get -u github.com/stamblerre/gocode &&\
    go get -u github.com/rogpeppe/godef &&\
    go get -u github.com/sqs/goreturns &&\
    go get -u golang.org/x/lint/golint &&\
    go get -u golang.org/x/tools/cmd/goimports &&\
    go get -u golang.org/x/tools/gopls &&\
    go get -u golang.org/x/tools/cmd/guru &&\
    go get -u golang.org/x/tools/cmd/gorename &&\
    go get -v github.com/derekparker/delve/cmd/dlv

# vscode extension
RUN code-server --install-extension ms-vscode.go