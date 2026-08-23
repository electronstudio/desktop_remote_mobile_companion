#!/bin/bash
DESTDIR=/usr
BINDIR=/bin
APPDIR=/share/applications
ICONDIR=/share/icons/hicolor
sudo install -Dm755 inara $DESTDIR$BINDIR/inara
sudo install -Dm755 inara_gui $DESTDIR$BINDIR/inara_gui
sudo setcap cap_sys_admin,cap_dac_override,cap_setpcap=p $DESTDIR$BINDIR/inara
sudo setcap cap_sys_admin,cap_dac_override,cap_setpcap=p  $DESTDIR$BINDIR/inara_gui
sudo mkdir -p $DESTDIR$APPDIR
sed 's|@BINDIR@|$BINDIR|' inara.desktop.in | sudo tee $DESTDIR$APPDIR/inara.desktop >/dev/null
sudo install -Dm644 icon-512.png  $DESTDIR$ICONDIR/512x512/apps/inara.png
sudo gtk-update-icon-cache -f $DESTDIR$ICONDIR 2>/dev/null || true
sudo update-desktop-database $DESTDIR$APPDIR 2>/dev/null || true
