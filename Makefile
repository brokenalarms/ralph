PREFIX   ?= $(HOME)/.local
BINDIR   ?= $(PREFIX)/bin
BINARY   := ralph

.PHONY: build install uninstall test clean

build:
	go build -C go -o ../$(BINARY) ./cmd/ralph

test:
	go test -C go ./... -count=1 -failfast

install: build
	install -d $(BINDIR)
	install -m 755 $(BINARY) $(BINDIR)/$(BINARY)

uninstall:
	rm -f $(BINDIR)/$(BINARY)

clean:
	rm -f $(BINARY)
