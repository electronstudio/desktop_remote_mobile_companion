//go:build linux

package main

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
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
