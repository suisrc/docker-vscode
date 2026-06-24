.PHONY: build clean

authz: main.go login.html logout.html go.mod
	CGO_ENABLED=0 go build -ldflags="-s -w" -o authz .

build: authz

clean:
	rm -f authz
