PREFIX ?= /usr
BINDIR := $(PREFIX)/bin
APPDIR := $(PREFIX)/share/applications
ICONDIR := $(PREFIX)/share/icons/hicolor
SYSTEMDUSERDIR := $(PREFIX)/lib/systemd/user

.PHONY: all install clean distclean appimage

# ---- FFmpeg 8.1.2 (statically linked into the single binary) ----
# The system FFmpeg version may be incompatible with go-astiav; we build 8.1.2
# statically and link it in, so the resulting binary needs no libav*.so.*
# installed on the user's system. FFmpeg's own deps (libdrm, libva, libx264,
# X11, ...) still link dynamically and are handled by dpkg-shlibdeps.
# Download/build only happens when ffmpeg-8.1.2/_install/ is missing.
FFMPEG_VERSION := 8.1.2
FFMPEG_DIR     := ffmpeg-$(FFMPEG_VERSION)
FFMPEG_PREFIX  := $(CURDIR)/$(FFMPEG_DIR)/_install
FFMPEG_TARBALL := ffmpeg-$(FFMPEG_VERSION).tar.xz
FFMPEG_URL     := https://ffmpeg.org/releases/$(FFMPEG_TARBALL)
# Configure marker: present => FFmpeg already built+installed, skip everything.
FFMPEG_PC      := $(FFMPEG_PREFIX)/lib/pkgconfig/libavcodec.pc
FFMPEG_CONFIGURE_FLAGS := \
	--prefix=$(FFMPEG_PREFIX) \
	--disable-shared --enable-static --enable-pic \
	--enable-gpl --enable-version3 \
	--enable-libx264 --enable-libdrm --enable-vaapi \
	--disable-doc --disable-programs --disable-ffplay --disable-ffprobe

# Env for Go builds: point pkg-config at our static 8.1.2 and request static libs.
FFMPEG_ENV := PKG_CONFIG_PATH=$(FFMPEG_PREFIX)/lib/pkgconfig PKG_CONFIG_FLAGS=--static

# Sources and embedded assets that should trigger a rebuild
SOURCES := $(shell find . -name '*.go' -not -path './third_party/*') go.mod go.sum \
	$(shell find server/static -type f) server/VERSION

all: inara_gui inara

# $(FFMPEG_PC): download + build + install FFmpeg 8.1.2 into $(FFMPEG_PREFIX).
# Skipped entirely once $(FFMPEG_PC) exists.
$(FFMPEG_PC):
	@if [ ! -f $(FFMPEG_TARBALL) ]; then \
		echo "Downloading $(FFMPEG_URL)"; \
		curl -fL --retry 3 -o $(FFMPEG_TARBALL) $(FFMPEG_URL); \
	fi
	@if [ ! -d $(FFMPEG_DIR) ]; then \
		echo "Extracting $(FFMPEG_TARBALL)"; \
		tar xf $(FFMPEG_TARBALL); \
	fi
	cd $(FFMPEG_DIR) && (make distclean >/dev/null 2>&1 || true) && \
		./configure $(FFMPEG_CONFIGURE_FLAGS) && \
		$(MAKE) -j$$(nproc) && \
		$(MAKE) install

inara_gui: $(SOURCES) $(FFMPEG_PC)
	$(FFMPEG_ENV) go build -tags migrated_fynedo -o inara_gui ./cmd/inara_gui

inara: $(SOURCES) $(FFMPEG_PC)
	$(FFMPEG_ENV) go build -o inara ./cmd/inara

install: all
	install -Dm755 inara $(DESTDIR)$(BINDIR)/inara
	install -Dm755 inara_gui $(DESTDIR)$(BINDIR)/inara_gui
	setcap cap_sys_admin,cap_dac_override,cap_setpcap=p $(DESTDIR)$(BINDIR)/inara
	setcap cap_sys_admin,cap_dac_override,cap_setpcap=p  $(DESTDIR)$(BINDIR)/inara_gui
	mkdir -p $(DESTDIR)$(APPDIR)
	sed 's|@BINDIR@|$(BINDIR)|' inara.desktop.in > $(DESTDIR)$(APPDIR)/inara.desktop
	mkdir -p $(DESTDIR)$(SYSTEMDUSERDIR)
	sed 's|@BINDIR@|$(BINDIR)|' inara.service.in > $(DESTDIR)$(SYSTEMDUSERDIR)/inara.service
	install -Dm644 server/static/icon-512.png  $(DESTDIR)$(ICONDIR)/512x512/apps/inara.png
	gtk-update-icon-cache -f $(DESTDIR)$(ICONDIR) 2>/dev/null || true
	update-desktop-database $(DESTDIR)$(APPDIR) 2>/dev/null || true

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/inara $(DESTDIR)$(BINDIR)/inara_gui $(DESTDIR)$(APPDIR)/inara.desktop
	rm -f $(DESTDIR)$(SYSTEMDUSERDIR)/inara.service
	rm -f $(DESTDIR)$(ICONDIR)/512x512/apps/inara.png
	gtk-update-icon-cache -f $(DESTDIR)$(ICONDIR) 2>/dev/null || true
	update-desktop-database $(DESTDIR)$(APPDIR) 2>/dev/null || true



# Build an AppImage containing inara_gui (no bundled shared libs; the host
# provides glibc and the X11/GL/Wayland client libraries, same as the plain
# binary release). Output goes to dist/.
appimage: inara_gui
	./scripts/make-appimage.sh inara_gui $$(cat server/VERSION)

clean:
	rm -f inara_gui inara

distclean: clean
	rm -rf $(FFMPEG_DIR) $(FFMPEG_TARBALL)
