PREFIX ?= /usr
BINDIR := $(PREFIX)/bin
APPDIR := $(PREFIX)/share/applications
ICONDIR := $(PREFIX)/share/icons/hicolor

.PHONY: all install clean

# Sources and embedded assets that should trigger a rebuild
SOURCES := $(shell find . -name '*.go' -not -path './third_party/*') go.mod go.sum \
	$(shell find server/static -type f) server/VERSION

all: companion_gui companion

companion_gui: $(SOURCES)
	go build -tags migrated_fynedo -o companion_gui ./cmd/companion_gui

companion: $(SOURCES)
	go build -o companion ./cmd/companion

install: all
	install -Dm755 companion $(DESTDIR)$(BINDIR)/companion
	install -Dm755 companion_gui $(DESTDIR)$(BINDIR)/companion_gui
	setcap cap_sys_admin,cap_dac_override,cap_setpcap=p $(DESTDIR)$(BINDIR)/companion
	setcap cap_sys_admin,cap_dac_override,cap_setpcap=p  $(DESTDIR)$(BINDIR)/companion_gui
	mkdir -p $(DESTDIR)$(APPDIR)
	sed 's|@BINDIR@|$(BINDIR)|' companion.desktop.in > $(DESTDIR)$(APPDIR)/companion.desktop
	install -Dm644 server/static/icon-512.png  $(DESTDIR)$(ICONDIR)/512x512/apps/companion.png
	gtk-update-icon-cache -f $(DESTDIR)$(ICONDIR) 2>/dev/null || true
	update-desktop-database $(DESTDIR)$(APPDIR) 2>/dev/null || true

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/companion $(DESTDIR)$(BINDIR)/companion_gui $(DESTDIR)$(APPDIR)/companion.desktop
	rm -f $(DESTDIR)$(ICONDIR)/512x512/apps/companion.png
	gtk-update-icon-cache -f $(DESTDIR)$(ICONDIR) 2>/dev/null || true
	update-desktop-database $(DESTDIR)$(APPDIR) 2>/dev/null || true



clean:
	rm -f companion_gui companion
