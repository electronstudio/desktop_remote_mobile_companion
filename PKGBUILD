# Maintainer: Richard Smith <aur@electronstudio.co.uk>
pkgname=desktop-remote-mobile-companion-git
pkgver=0  # placeholder, overwritten by pkgver()
pkgrel=1
pkgdesc="Connect your mobile device wirelessly to your PC and use it as trackpad, graphics tablet, etc"
arch=('x86_64')
url="https://github.com/electronstudio/desktop_remote_mobile_companion"
license=('GPL-3.0-only')
depends=('x264' 'gcc-libs' 'libglvnd' 'mesa' 'libx11' 'libxcursor' 'libxrandr' 'libxinerama' 'libxi' 'wayland')
makedepends=('go>=1.24')
source=(
  "git+https://github.com/electronstudio/desktop_remote_mobile_companion.git"
)
sha256sums=('SKIP')

pkgver() {
  cd "$srcdir/desktop_remote_mobile_companion"
  local base=$(cat server/VERSION)
  printf "%s.r%s.%s" "$base" "$(git rev-list --count HEAD)" "$(git rev-parse --short HEAD)"
}

build() {
  cd "$srcdir/desktop_remote_mobile_companion"
  export GOFLAGS=-buildvcs=false
  go build -o companion ./cmd/companion
  go build -tags migrated_fynedo -o companion_gui ./cmd/companion_gui
}

package() {
  cd "$srcdir/desktop_remote_mobile_companion"
  make -f Makefile install DESTDIR="$pkgdir" PREFIX=/usr
  rm -f "$pkgdir/usr/share/applications/mimeinfo.cache"
  rm -f "$pkgdir/usr/share/icons/hicolor/icon-theme.cache"
}
