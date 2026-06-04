VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS = -s -w -X main.version=$(VERSION)

.PHONY: build build-linux build-all test clean

build:
	go build -ldflags="$(LDFLAGS)" -o xrn-install ./cmd/xrn-install
	go build -ldflags="$(LDFLAGS)" -o xrnctl ./cmd/xrnctl

build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o xrn-install-linux-amd64 ./cmd/xrn-install
	GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o xrn-install-linux-arm64 ./cmd/xrn-install
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o xrnctl-linux-amd64 ./cmd/xrnctl
	GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o xrnctl-linux-arm64 ./cmd/xrnctl

build-all: build build-linux

test:
	go test -v ./...

clean:
	rm -f xrn-install xrn-install-linux-amd64 xrn-install-linux-arm64
	rm -f xrnctl xrnctl-linux-amd64 xrnctl-linux-arm64
