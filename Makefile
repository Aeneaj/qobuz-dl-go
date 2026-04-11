BINARY   := qobuz-dl
CMD      := ./cmd/qobuz-dl
DIST     := dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -s -w -X main.version=$(VERSION)

PLATFORMS := \
	linux/amd64 \
	linux/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64

.PHONY: all clean checksums

all: clean $(PLATFORMS) checksums

clean:
	rm -rf $(DIST)
	mkdir -p $(DIST)

$(PLATFORMS):
	$(eval OS   := $(word 1,$(subst /, ,$@)))
	$(eval ARCH := $(word 2,$(subst /, ,$@)))
	$(eval EXT  := $(if $(filter windows,$(OS)),.exe,))
	$(eval OUT  := $(DIST)/$(BINARY)-$(OS)-$(ARCH)$(EXT))
	GOOS=$(OS) GOARCH=$(ARCH) go build -ldflags "$(LDFLAGS)" -o $(OUT) $(CMD)
	@echo "Built $(OUT)"

checksums:
	cd $(DIST) && sha256sum * > checksums.txt
	@echo "Checksums written to $(DIST)/checksums.txt"

# Quick local build (current OS/arch)
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test:
	go test ./...

vet:
	go vet ./...
