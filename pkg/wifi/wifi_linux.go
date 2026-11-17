//go:build linux

// Package wifi provides nl80211-based Wi-Fi management operations for Linux.
package wifi

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

const (
	genlIDCtrl         = 16
	ctrlCmdGetFamily   = 3
	ctrlAttrFamilyName = 2
	ctrlAttrFamilyID   = 1

	nl80211CmdSetPowerSave = 61
	nl80211CmdGetPowerSave = 62
	nl80211AttrIfindex     = 3
	nl80211AttrPsState     = 93
	nl80211PsDisabled      = 0
	nl80211PsEnabled       = 1
)

type nlmsghdr struct {
	Len   uint32
	Type  uint16
	Flags uint16
	Seq   uint32
	Pid   uint32
}

type genlmsghdr struct {
	Cmd      uint8
	Version  uint8
	Reserved uint16
}

type nlattr struct {
	Len  uint16
	Type uint16
}

type nlmsgerr struct {
	Error int32
	Msg   nlmsghdr
}

func align4(n int) int {
	return (n + 3) & ^3
}

func getFamilyID(fd int, family string) (uint16, error) {
	nameBytes := append([]byte(family), 0)
	attrLen := 4 + len(nameBytes)
	msgLen := 16 + 4 + align4(attrLen)

	buf := make([]byte, msgLen)
	nlhdr := (*nlmsghdr)(unsafe.Pointer(&buf[0]))
	nlhdr.Len = uint32(msgLen)
	nlhdr.Type = genlIDCtrl
	nlhdr.Flags = syscall.NLM_F_REQUEST
	nlhdr.Seq = 1

	genlhdr := (*genlmsghdr)(unsafe.Pointer(&buf[16]))
	genlhdr.Cmd = ctrlCmdGetFamily
	genlhdr.Version = 1

	attr := (*nlattr)(unsafe.Pointer(&buf[20]))
	attr.Len = uint16(attrLen)
	attr.Type = ctrlAttrFamilyName
	copy(buf[24:], nameBytes)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(fd, buf, 0, sa); err != nil {
		return 0, fmt.Errorf("nl80211: send family request: %w", err)
	}

	recvBuf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, recvBuf, 0)
	if err != nil {
		return 0, fmt.Errorf("nl80211: recv family response: %w", err)
	}
	if n < 20 {
		return 0, fmt.Errorf("nl80211: short family response (%d bytes)", n)
	}

	offset := 20
	for offset+4 <= n {
		aLen := int(binary.LittleEndian.Uint16(recvBuf[offset : offset+2]))
		aType := binary.LittleEndian.Uint16(recvBuf[offset+2 : offset+4])
		if aLen < 4 || offset+aLen > n {
			break
		}
		if aType == ctrlAttrFamilyID && aLen >= 6 {
			return binary.LittleEndian.Uint16(recvBuf[offset+4 : offset+6]), nil
		}
		offset += align4(aLen)
	}
	return 0, fmt.Errorf("nl80211: family %q not found", family)
}

func openSocket() (int, uint16, error) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_GENERIC)
	if err != nil {
		return -1, 0, fmt.Errorf("nl80211: socket: %w", err)
	}
	tv := syscall.NsecToTimeval(3 * int64(time.Second))
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
	_ = syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &tv)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.Close(fd)
		return -1, 0, fmt.Errorf("nl80211: bind: %w", err)
	}

	famID, err := getFamilyID(fd, "nl80211")
	if err != nil {
		syscall.Close(fd)
		return -1, 0, err
	}
	return fd, famID, nil
}

// GetPowerSave returns whether Wi-Fi power saving is currently enabled on the given interface.
func GetPowerSave(ifname string) (bool, error) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return false, fmt.Errorf("nl80211: interface %s: %w", ifname, err)
	}

	fd, famID, err := openSocket()
	if err != nil {
		return false, err
	}
	defer syscall.Close(fd)

	msgLen := 16 + 4 + 8 // nlmsghdr (16) + genlmsghdr (4) + attr ifindex (8)
	buf := make([]byte, msgLen)
	nlhdr := (*nlmsghdr)(unsafe.Pointer(&buf[0]))
	nlhdr.Len = uint32(msgLen)
	nlhdr.Type = famID
	nlhdr.Flags = syscall.NLM_F_REQUEST
	nlhdr.Seq = 2

	genlhdr := (*genlmsghdr)(unsafe.Pointer(&buf[16]))
	genlhdr.Cmd = nl80211CmdGetPowerSave
	genlhdr.Version = 1

	attr := (*nlattr)(unsafe.Pointer(&buf[20]))
	attr.Len = 8
	attr.Type = nl80211AttrIfindex
	binary.LittleEndian.PutUint32(buf[24:], uint32(ifi.Index))

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(fd, buf, 0, sa); err != nil {
		return false, fmt.Errorf("nl80211: send get_power_save: %w", err)
	}

	recvBuf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, recvBuf, 0)
	if err != nil {
		return false, fmt.Errorf("nl80211: recv get_power_save: %w", err)
	}
	if n < 16 {
		return false, fmt.Errorf("nl80211: short response (%d bytes)", n)
	}

	nlhdrResp := (*nlmsghdr)(unsafe.Pointer(&recvBuf[0]))
	if nlhdrResp.Type == syscall.NLMSG_ERROR {
		if n >= 20 {
			errResp := (*nlmsgerr)(unsafe.Pointer(&recvBuf[16]))
			if errResp.Error != 0 {
				return false, fmt.Errorf("nl80211: get_power_save %s: %w", ifname, syscall.Errno(-errResp.Error))
			}
		}
		return false, fmt.Errorf("nl80211: error response")
	}

	offset := 20
	for offset+4 <= n {
		aLen := int(binary.LittleEndian.Uint16(recvBuf[offset : offset+2]))
		aType := binary.LittleEndian.Uint16(recvBuf[offset+2 : offset+4])
		if aLen < 4 || offset+aLen > n {
			break
		}
		if aType == nl80211AttrPsState && aLen >= 8 {
			state := binary.LittleEndian.Uint32(recvBuf[offset+4 : offset+8])
			return state == nl80211PsEnabled, nil
		}
		offset += align4(aLen)
	}
	return false, fmt.Errorf("nl80211: get_power_save %s: attribute missing in response", ifname)
}

// SetPowerSave switches Wi-Fi power saving on or off on the given interface.
func SetPowerSave(ifname string, enable bool) error {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return fmt.Errorf("nl80211: interface %s: %w", ifname, err)
	}

	fd, famID, err := openSocket()
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	msgLen := 16 + 4 + 8 + 8 // nlmsghdr (16) + genlmsghdr (4) + ifindex attr (8) + ps_state attr (8)
	buf := make([]byte, msgLen)
	nlhdr := (*nlmsghdr)(unsafe.Pointer(&buf[0]))
	nlhdr.Len = uint32(msgLen)
	nlhdr.Type = famID
	nlhdr.Flags = syscall.NLM_F_REQUEST | syscall.NLM_F_ACK
	nlhdr.Seq = 3

	genlhdr := (*genlmsghdr)(unsafe.Pointer(&buf[16]))
	genlhdr.Cmd = nl80211CmdSetPowerSave
	genlhdr.Version = 1

	// Attr 1: ifindex
	attr1 := (*nlattr)(unsafe.Pointer(&buf[20]))
	attr1.Len = 8
	attr1.Type = nl80211AttrIfindex
	binary.LittleEndian.PutUint32(buf[24:], uint32(ifi.Index))

	// Attr 2: ps_state
	attr2 := (*nlattr)(unsafe.Pointer(&buf[28]))
	attr2.Len = 8
	attr2.Type = nl80211AttrPsState
	var state uint32 = nl80211PsDisabled
	if enable {
		state = nl80211PsEnabled
	}
	binary.LittleEndian.PutUint32(buf[32:], state)

	sa := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(fd, buf, 0, sa); err != nil {
		return fmt.Errorf("nl80211: send set_power_save: %w", err)
	}

	recvBuf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, recvBuf, 0)
	if err != nil {
		return fmt.Errorf("nl80211: recv set_power_save ack: %w", err)
	}
	if n < 16 {
		return fmt.Errorf("nl80211: short ack response (%d bytes)", n)
	}

	nlhdrResp := (*nlmsghdr)(unsafe.Pointer(&recvBuf[0]))
	if nlhdrResp.Type == syscall.NLMSG_ERROR {
		if n >= 20 {
			errResp := (*nlmsgerr)(unsafe.Pointer(&recvBuf[16]))
			if errResp.Error != 0 {
				return fmt.Errorf("nl80211: set_power_save %s: %w", ifname, syscall.Errno(-errResp.Error))
			}
			return nil
		}
		return fmt.Errorf("nl80211: short error response")
	}
	return nil
}
