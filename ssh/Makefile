.PHONY: build clean

sshp: main.go go.mod
	CGO_ENABLED=0 go build -ldflags="-s -w" -o sshp .

build: sshp

clean:
	rm -f sshp
