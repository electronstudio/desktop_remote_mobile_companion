# Inara: the Desktop Remote Mobile Companion

[![AI-DECLARATION: copilot](https://img.shields.io/badge/䷼%20AI--DECLARATION-copilot-fee2e2?labelColor=fee2e2)](https://ai-declaration.md)

Connect your mobile device wirelessly to your PC and use it as:

| Feature               | Linux Wayland  | Linux X11    | Windows | Macos |
|-----------------------|----------------|--------------|---------|-------|
| Trackpad              | ✅             | ✅           | ❌      | ❌    |
| Graphics tablet       | ✅ (almost!)   | ✅ (almost!) | ❌      | ❌    |
| Display mirror        | ✅ (Intel/AMD) | ✅           | ❌      | ❌    |
| ~~Camera~~            | ❌             | ❌           | ❌      | ❌    |
| ~~Microphone~~        | ❌             | ❌           | ❌      | ❌    |
| ~~Keyboard~~          | ❌             | ❌           | ❌      | ❌    |
| ~~Clipboard~~         | ❌             | ❌           | ❌      | ❌    |
| ~~Extra display~~     | ❌             | ❌           | ❌      | ❌    |
| ~~Game controller~~   | ❌             | ❌           | ❌      | ❌    |
| ~~File send/receive~~ | ❌             | ❌           | ❌      | ❌    |

Tested desktops: Gnome Wayland, XFCE X11.

Tested software: Gimp, Krita

Tested mobile devices: iPhone, iPad + Apple Pencil.

## Compared to Weylus

[Weylus](https://github.com/electronstudio/WeylusCommunityEdition) is a program for graphic tablet
and display mirroring.  Inara explores some _different_ methods of implementing those features,
which may not necessarily be better!

Inara uses WebRTC connections. The underlying protocol is UDP rather than TCP which _may_ (or may not)
have lower latency.

Inara mirrors the display using ffmpeg's kmsgrab, which _may_ (or may not) work better on Wayland (although doesn't
work at all on Nvidia systems).

Inara uses hardware accelerated VAAPI encoding.

Inara is written in Go rather than Rust.


## Install

From [releases](releases).

On Intel GPU systems the full drivers are required, install on Debian with:

    sudo apt install intel-media-va-driver-non-free

Also on Intel the `--low-power 1` option may improve latency.

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

To work, Inara requires access to `uinput`.  You have three options
for getting this:
1. Run with `sudo companion`. 
2. Add your user to the uniput group, then reboot. (This has the downside of granting access to every other program on your system.):

    `sudo usermod -aG uinput $USER`

3. Grant a more limited set of permissions to just this program permanently:

    `sudo /sbin/setcap cap_sys_admin,cap_dac_override,cap_setpcap=p /path/to/companion`

To capture video, Inara requires `sys_admin` permission.  Options for this:
1. Disable video with `companion --video-source none`
2. Run with `sudo companion`.
3. Grant a more limited set of permissions to the program permanently:

   `sudo /sbin/setcap cap_sys_admin,cap_dac_override,cap_setpcap=p /path/to/companion`


## Run

```bash
./companion --port 8080
```

The first time it runs, a self-signed certificate is created in your user cache
directory (`$HOME/.cache/desktop_remote_mobile_companion` on Linux).

It will print a URL and also a QR code for that URL, e.g. `https://192.168.1.150:8080`. 
  Open this URL on your mobile device.  You will need to accept the self-signed certificate.

Add a bookmark to your homescreen to access the app fullscreen and allow
swipes to work properly.

Swipe left/right from screen edge to switch tools.  Rotate
device to landscape orientation to use tool fullscreen.


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
go build -o companion
```


