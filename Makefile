PREFIX   ?= $(HOME)/.local
BINDIR   ?= $(PREFIX)/bin
BINARY   := ralph
VERSION  := $(shell git describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo v0.1.0-dev)
LDFLAGS  := -X github.com/brokenalarms/ralph/internal/config.Version=$(VERSION:v%=%)

.PHONY: build install uninstall test clean

build:
	go build -C go -ldflags '$(LDFLAGS)' -o ../$(BINARY) ./cmd/ralph

test:
	go test -C go ./... -count=1 -failfast

install: build
	install -d $(BINDIR)
	install -m 755 $(BINARY) $(BINDIR)/$(BINARY)

uninstall:
	rm -f $(BINDIR)/$(BINARY)

clean:
	rm -f $(BINARY)
