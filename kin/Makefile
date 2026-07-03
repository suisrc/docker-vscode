.PHONY: build clean

kin: main.go login.html logout.html loading.html go.mod
	CGO_ENABLED=0 go build -ldflags="-s -w" -o kin .

build: kin

clean:
	rm -f kin
