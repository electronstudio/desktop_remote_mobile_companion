//go:build linux

package main

const uinputInstructions = `uinput access denied. To fix permissions, run:

  echo 'KERNEL=="uinput", MODE="0660", GROUP="input"' | sudo tee /etc/udev/rules.d/99-uinput.rules
  sudo udevadm control --reload
  sudo udevadm trigger

Then make sure your user is in the 'input' group:

  sudo usermod -aG input $USER

Log out and log back in for the group change to take effect.`
