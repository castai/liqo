// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build linux

package nodeagent

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// SIOCETHTOOL performs ethtool ioctls on a network interface.
	siocethtool = 0x8946
	// ETHTOOL_STXCSUM toggles TX checksum offload.
	ethtoolSTXCSUM = 0x00000017
)

type ethtoolValue struct {
	Cmd  uint32
	Data uint32
}

type ifreqData struct {
	Name [unix.IFNAMSIZ]byte
	Data uintptr
}

func disableTXChecksumOffload(iface string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("opening ethtool socket: %w", err)
	}
	defer unix.Close(fd)

	value := ethtoolValue{
		Cmd:  ethtoolSTXCSUM,
		Data: 0,
	}
	var req ifreqData
	copy(req.Name[:], iface)
	req.Data = uintptr(unsafe.Pointer(&value))

	if err := unix.IoctlSetInt(fd, siocethtool, int(uintptr(unsafe.Pointer(&req)))); err != nil {
		return fmt.Errorf("disabling tx checksum offload on %s: %w", iface, err)
	}

	return nil
}