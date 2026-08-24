# Maintainer: Richard Smith <aur@electronstudio.co.uk>
pkgname=inara
conflicts=('inara-git')
pkgver=0.7.0
pkgrel=1
pkgdesc="Use mobile device as trackpad, graphics tablet, for PC."
arch=('x86_64')
url="https://github.com/electronstudio/desktop_remote_mobile_companion"
license=('GPL-3.0-only')
depends=('x264' 'gcc-libs' 'libglvnd' 'mesa' 'libx11' 'libxcursor' 'libxrandr' 'libxinerama' 'libxi' 'wayland' 'libxkbcommon' 'libvdpau' 'alsa-lib' 'hicolor-icon-theme' 'libva' 'sndio' 'libxv')
makedepends=('go>=1.26' 'nasm')
source=(
  "$pkgname-$pkgver.tar.gz::https://github.com/electronstudio/desktop_remote_mobile_companion/archive/refs/tags/v$pkgver.tar.gz"
)
sha256sums=('d7bf6223265322d1d9f5d5421f91434a406391bbdee14b95b3d0b36ed7118045')

build() {
  cd "$srcdir/desktop_remote_mobile_companion-$pkgver"
  export GOFLAGS=-buildvcs=false
  export CGO_CFLAGS=""
  export CGO_LDFLAGS=""
  export CFLAGS="-march=native -O3"
  export CXXFLAGS="-march=native -O3"
  export LDFLAGS=""
  export LTOFLAGS=""
  make -f Makefile
}

package() {
  cd "$srcdir/desktop_remote_mobile_companion-$pkgver"
  make -f Makefile install DESTDIR="$pkgdir" PREFIX=/usr
  rm -f "$pkgdir/usr/share/applications/mimeinfo.cache"
  rm -f "$pkgdir/usr/share/icons/hicolor/icon-theme.cache"
}
