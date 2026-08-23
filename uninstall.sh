#!/bin/bash
DESTDIR=/usr
BINDIR=/bin
APPDIR=/share/applications
ICONDIR=/share/icons/hicolor
sudo rm $DESTDIR$BINDIR/inara
sudo rm $DESTDIR$BINDIR/inara_gui
sudo rm $DESTDIR$APPDIR/inara.desktop
sudo rm $DESTDIR$ICONDIR/512x512/apps/inara.png
sudo gtk-update-icon-cache -f $DESTDIR$ICONDIR 2>/dev/null || true
sudo update-desktop-database $DESTDIR$APPDIR 2>/dev/null || true
