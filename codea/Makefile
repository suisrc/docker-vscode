.PHONY: build clean

codea: main.go login.html logout.html loading.html go.mod
	CGO_ENABLED=0 go build -ldflags="-s -w" -o codea .

build: codea

clean:
	rm -f codea
