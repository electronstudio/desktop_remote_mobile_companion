package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/fatih/color"
	"golang.org/x/sys/unix"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// capSysAdminBit is the bit position of CAP_SYS_ADMIN in the Linux capability
// bitmask. CAP_SYS_ADMIN is capability number 21.
const capSysAdminBit = 21

// hasCapSysAdmin reports whether the current process has CAP_SYS_ADMIN in its
// effective capability set. kmsgrab needs CAP_SYS_ADMIN to acquire DRM master
// and map the framebuffer, so without it desktop video capture fails with
// EINVAL ("No handle set on framebuffer").
//
// The check is dependency-free: it reads the CapEff line from
// /proc/self/status, which is the effective capabilities as a 64-bit hex mask.
func hasCapSysAdmin() (bool, error) {
	orig := cap.GetProc()
	defer orig.SetProc()
	c, err := orig.Dup()
	if err == nil {
		if err := c.SetFlag(cap.Effective, true, cap.SYS_ADMIN); err == nil {
			c.SetProc()
		}
	}

	f, err := os.Open("/proc/self/status")
	if err != nil {
		return false, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "CapEff:\t") {
			continue
		}
		hex := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		mask, err := strconv.ParseUint(hex, 16, 64)
		if err != nil {
			return false, err
		}
		return mask&(1<<capSysAdminBit) != 0, nil
	}
	if err := s.Err(); err != nil {
		return false, err
	}
	return false, errors.New("CapEff not found in /proc/self/status")
}

// onNoSuidMount reports whether the executable's filesystem is mounted with
// the MS_NOSUID flag. On a nosuid mount the kernel ignores the
// security.capability xattr, so `setcap cap_sys_admin+ep <binary>` writes the
// attribute (getcap shows it) but the capability is never granted at exec
// time. This is a common reason setcap appears to do nothing.
//
// If the executable path cannot be determined or statfs fails, it returns
// false and the error; callers should fall back to the generic setcap advice.
func onNoSuidMount() (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		// os.Executable may fail on some platforms but not on Linux; if it
		// somehow does, fall back to argv[0]-ish behavior via /proc/self/exe.
		exe = "/proc/self/exe"
	}
	var st unix.Statfs_t
	if err := unix.Statfs(exe, &st); err != nil {
		return false, err
	}
	return st.Flags&unix.ST_NOSUID != 0, nil
}

// dropSudoPrivileges drops privileges back to the user who invoked sudo.
// If the program wasn't run via sudo, it does nothing.
func dropSudoPrivileges() error {
	// We only want to drop privileges if we are actually root
	if os.Getuid() != 0 {
		return nil
	}

	sudoUID := os.Getenv("SUDO_UID")
	sudoGID := os.Getenv("SUDO_GID")

	// If SUDO_UID is empty, the program was started by root directly, not via sudo.
	if sudoUID == "" {
		return fmt.Errorf("could not determine original user: SUDO_UID is not set")
	}

	uid, err := strconv.Atoi(sudoUID)
	if err != nil {
		return fmt.Errorf("invalid SUDO_UID: %w", err)
	}

	// Parse SUDO_GID. If it's missing for some reason, we can look it up
	// using the SUDO_USER environment variable and the os/user package.
	gid, err := strconv.Atoi(sudoGID)
	if err != nil {
		sudoUser := os.Getenv("SUDO_USER")
		if sudoUser == "" {
			return fmt.Errorf("could not determine group: SUDO_GID is invalid and SUDO_USER is missing")
		}

		u, err := user.Lookup(sudoUser)
		if err != nil {
			return fmt.Errorf("failed to lookup user %s: %w", sudoUser, err)
		}

		gid, err = strconv.Atoi(u.Gid)
		if err != nil {
			return fmt.Errorf("invalid GID for user %s: %w", sudoUser, err)
		}
	}

	// 1. Clear supplementary groups (crucial for security)
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}

	// 2. Drop group privileges
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}

	// 3. Drop user privileges
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}

	return nil
}

func reExecWithSudo() {
	color.Set(color.FgCyan)
	fmt.Printf("\n\nAttempting to re-run with sudo privileges...\n\n")
	color.Unset()
	// Get the absolute path of the currently running executable
	executable, err := os.Executable()
	if err != nil {
		fmt.Printf("Error determining executable path: %v\n", err)
		os.Exit(1)
	}

	// Prepare the arguments for the new process.
	// We prepend the executable path to the existing arguments (skipping the original program name).
	args := append([]string{executable}, os.Args[1:]...)

	// Create the exec.Cmd to run the command via sudo
	cmd := exec.Command("sudo", args...)

	// Connect the standard input, output, and error streams so the user can
	// interact with the program (and enter their sudo password if prompted).
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the command. This blocks until the child process exits.
	err = cmd.Run()
	if err != nil {
		// Check if the error is due to the user canceling the sudo prompt
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if status.Signaled() && status.Signal() == syscall.SIGINT {
					fmt.Println("\nProcess interrupted by user.")
					os.Exit(1)
				}
			}
		}
		fmt.Printf("Failed to execute with sudo: %v\n", err)
		os.Exit(1)
	}

	// Exit with the same exit code as the child process
	if cmd.ProcessState != nil {
		os.Exit(cmd.ProcessState.ExitCode())
	}
	os.Exit(0)
}
