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
	sudo cp companion_gui companion /usr/bin/
	sudo setcap cap_sys_admin,cap_dac_override,cap_setpcap=p /usr/bin/companion_gui
	sudo setcap cap_sys_admin,cap_dac_override,cap_setpcap=p /usr/bin/companion

clean:
	rm -f companion_gui companion
