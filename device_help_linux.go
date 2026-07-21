//go:build linux

package main

const uinputInstructions = `uinput access denied. To fix permissions, run:

  echo 'KERNEL=="uinput", MODE="0660", GROUP="input"' | sudo tee /etc/udev/rules.d/99-uinput.rules
  sudo udevadm control --reload
  sudo udevadm trigger

Then make sure your user is in the 'input' group:

  sudo usermod -aG input $USER

Log out and log back in for the group change to take effect.`

const videoMissingCapInstructions = `To enable desktop video capture, grant CAP_SYS_ADMIN to the binary once (no need to run as root afterwards):

  sudo setcap cap_sys_admin+ep <executable_path>

Note: you must re-run setcap after every rebuild, since it is stored as a file
extended attribute and is lost when the file is replaced. Alternatively, run
the program with --no-video to use only the trackpad/tablet without desktop
streaming.`
