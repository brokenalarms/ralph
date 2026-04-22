PREFIX   ?= $(HOME)/.local
BINDIR   ?= $(PREFIX)/bin
BINARY   := ralph
VERSION  := $(shell node -e "process.stdout.write(JSON.parse(require('fs').readFileSync('package.json','utf8')).version)" 2>/dev/null || echo 0.1.0-dev)
LDFLAGS  := -X github.com/brokenalarms/ralph/internal/config.Version=$(VERSION)

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
