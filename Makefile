PREFIX ?= /usr/local

install:
	install -d $(PREFIX)/bin
	install -m 755 ralph.sh $(PREFIX)/bin/ralph

uninstall:
	rm -f $(PREFIX)/bin/ralph
