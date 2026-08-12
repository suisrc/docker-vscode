.PHONY: build clean

kss: main.go go.mod
	CGO_ENABLED=0 go build -ldflags="-s -w" -o kss .

build: kss

clean:
	rm -f kss
