# Maintainer: Richard Smith <aur@electronstudio.co.uk>
pkgname=desktop-remote-mobile-companion-git
pkgver=0  # placeholder, overwritten by pkgver()
pkgrel=1
pkgdesc="Connect your mobile device wirelessly to your PC and use it as trackpad, graphics tablet, etc"
arch=('x86_64')
url="https://github.com/electronstudio/desktop_remote_mobile_companion"
license=('GPL-3.0-only')
depends=('ffmpeg' 'gcc-libs')
makedepends=('go>=1.24')
install="arch.install"
source=(
  "git+https://github.com/electronstudio/desktop_remote_mobile_companion.git"
)
sha256sums=('SKIP')

pkgver() {
  cd "$srcdir/desktop_remote_mobile_companion"
  local base=$(cat VERSION)
  printf "%s.r%s.%s" "$base" "$(git rev-list --count HEAD)" "$(git rev-parse --short HEAD)"
}

build() {
  cd "$srcdir/desktop_remote_mobile_companion"
  export GOFLAGS=-buildvcs=false
  go build -o companion
}

package() {
  cd "$srcdir/desktop_remote_mobile_companion"
  mkdir -p "$pkgdir/usr/bin"
  cp companion "$pkgdir/usr/bin/companion"
  setcap cap_sys_admin+ep "$pkgdir/usr/bin/companion"
}
