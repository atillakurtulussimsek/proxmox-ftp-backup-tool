BINARY  := pve-ftp-backup
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
OUT     := build/$(BINARY)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet install dist clean

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

# Proxmox host üzerinde root olarak çalıştırın.
install: build
	./install.sh $(OUT)

# Proxmox'a kopyalanacak paket: binary + install.sh + systemd unit
dist: build
	mkdir -p build/dist/$(BINARY)
	cp $(OUT) install.sh deploy/$(BINARY).service README.md build/dist/$(BINARY)/
	tar -C build/dist -czf build/$(BINARY)-$(VERSION)-linux-amd64.tar.gz $(BINARY)
	@echo "Paket: build/$(BINARY)-$(VERSION)-linux-amd64.tar.gz"

clean:
	rm -rf build
