# Inara: the Desktop Remote Mobile Companion

[![AI-DECLARATION: copilot](https://img.shields.io/badge/䷼%20AI--DECLARATION-copilot-fee2e2?labelColor=fee2e2)](https://ai-declaration.md)

Connect your mobile device wirelessly to your PC and use it as:

| Feature               | Linux Wayland  | Linux X11 | Windows | Macos |
|-----------------------|----------------|-----------|---------|-------|
| Trackpad              | ✅             | ✅        | ❌      | ❌    |
| Graphics tablet       | ✅             | ✅        | ❌      | ❌    |
| Display mirror        | ✅ (Intel/AMD) | ✅        | ❌      | ❌    |
| ~~Camera~~            | ❌             | ❌        | ❌      | ❌    |
| ~~Microphone~~        | ❌             | ❌        | ❌      | ❌    |
| ~~Keyboard~~          | ❌             | ❌        | ❌      | ❌    |
| ~~Clipboard~~         | ❌             | ❌        | ❌      | ❌    |
| ~~Extra display~~     | ❌             | ❌        | ❌      | ❌    |
| ~~Game controller~~   | ❌             | ❌        | ❌      | ❌    |
| ~~File send/receive~~ | ❌             | ❌        | ❌      | ❌    |

Tested desktops: Gnome Wayland, XFCE X11.

Tested tablet software: Gimp, Krita

Tested mobile devices: iPhone, iPad + Apple Pencil.

## Compared to Weylus

[Weylus](https://github.com/electronstudio/WeylusCommunityEdition) is a program for graphic tablet
and display mirroring.  Inara explores some _different_ methods of implementing those features,
which may not necessarily be better!

Inara uses WebRTC connections. The underlying protocol is UDP rather than TCP which _may_ (or may not)
have lower latency.

Inara mirrors the display using ffmpeg's kmsgrab, which _may_ (or may not) work better on Wayland (although doesn't
work at all on Nvidia systems).

Inara uses hardware accelerated VAAPI/CUDA encoding.

Inara is written in Go rather than Rust.


## Install

Download from [releases](https://github.com/electronstudio/desktop_remote_mobile_companion/releases).  To install on Debian/Ubuntu:

    cd ~/Downloads
    sudo apt install ./desktop_remote_mobile_companion*.deb

On Intel GPU systems the full Intel drivers are required, e.g. install them on Debian with:

    sudo apt install intel-media-va-driver-non-free

If your OS firewall blocks connections by default (e.g. CachyOS) you will need to open a port, e.g.:

    sudo ufw allow 8080


## Run

For a GUI with options, run from your start menu, or run from terminal:

```bash
./companion_gui
```

Then click the `Start` button.  If it doesn't work, try clicking `Fix permissions`.

To run with no GUI:

```bash
./companion
```

The first time it runs, a self-signed certificate is created in your user cache
directory (`$HOME/.cache/desktop_remote_mobile_companion` on Linux).  Delete it to re-generate if
you have any certificate problems. (Also try using private browser tab.)

Inara will print a URL and also a QR code for that URL, e.g. `https://192.168.1.150:8080`. 
Open this URL on your mobile device.

On first run you must accept the self-signed certificate on the device and refresh the page.

Then video streaming will start:

![](docs/ipad1.jpg)

![](docs/ipad2.jpg) 

![](docs/ipad3.jpg) ![](docs/ipad4.jpg) 

The tools available so far are: graphics tablet, trackpad, log output.

![](docs/ipad5.jpg) ![](docs/ipad6.jpg) 

![](docs/ipad7.jpg)


## Build

### Arch (CachyOS, Artix, etc) Linux

```bash
git clone https://github.com/electronstudio/desktop_remote_mobile_companion.git
cd desktop_remote_mobile_companion
makepkg -si
```

### Debian, Ubuntu etc

```bash
sudo apt install libavcodec-dev libavfilter-dev libavformat-dev \
        libavutil-dev libavdevice-dev libdrm-dev
go build -o companion ./cmd/companion
```

To build the GUI (`companion_gui`) you additionally need the Fyne/GLFW
runtime libraries. On Debian:

```bash
sudo apt install libgl1 libegl1 libwayland-client0 libwayland-cursor0 \
        libwayland-egl1 libx11-6 libxcursor1 libxrandr2 libxinerama1 libxi6
make
```

## Advanced

If you can't see your mouse pointer, you need to disable hardware cursor:

    echo "MUTTER_DEBUG_DISABLE_HW_CURSORS=1" | sudo tee /etc/environment
    echo "KWIN_FORCE_SW_CURSOR=1" | sudo tee /etc/environment
    echo "WLR_NO_HARDWARE_CURSORS=1" | sudo tee /etc/environment
    sudo reboot

By default when the tablet tool is active it keeps the tablet 'hovering'
which prevents use of a mouse until you switch away from the tool.
This is required by some software such as Gnome.  But you can disable it:

    companion --dont-grab-mouse

## Security

To act as a trackpad or tablet, Inara requires access to `uinput`.  You have three options
for getting this:
1. Run with `sudo`. (You'll be prompted to do this if necessary.)
2. Add your user to the uniput group, then reboot. (This has the downside of granting access to every other program on your system.):

   `sudo usermod -aG uinput $USER`

3. Grant a more limited set of permissions to just this program permanently:

   `sudo /sbin/setcap cap_sys_admin,cap_dac_override,cap_setpcap=p /path/to/companion`

To capture video, Inara requires `sys_admin` permission.  Options for this:
1. Disable video with `companion --video-source none`
2. Run with `sudo`.
3. Grant a more limited set of permissions to the program permanently:

   `sudo /sbin/setcap cap_sys_admin,cap_dac_override,cap_setpcap=p /path/to/companion`

If you installed the Debian or Arch package, setcap will be done automatically.  If you downloaded the binary
tarball, run `companion_gui` and press 'fix permissions' to run setcap.
