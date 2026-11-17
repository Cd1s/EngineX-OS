//go:build linux

// Package wifi controls Linux Wi-Fi power saving through nl80211.
package wifi

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	genlIDCtrl             = 16
	ctrlCmdGetFamily       = 3
	ctrlAttrFamilyID       = 1
	ctrlAttrFamilyName     = 2
	nl80211CmdSetPowerSave = 61
	nl80211CmdGetPowerSave = 62
	nl80211AttrIfindex     = 3
	nl80211AttrPsState     = 93
)

func align4(n int) int { return (n + 3) &^ 3 }

func attribute(kind uint16, value []byte) []byte {
	b := make([]byte, align4(4+len(value)))
	binary.NativeEndian.PutUint16(b, uint16(4+len(value)))
	binary.NativeEndian.PutUint16(b[2:], kind)
	copy(b[4:], value)
	return b
}

func number(kind uint16, value uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, value)
	return attribute(kind, b)
}

func message(family uint16, command byte, seq uint32, ack bool, attrs ...[]byte) []byte {
	b := make([]byte, 20)
	binary.NativeEndian.PutUint16(b[4:], family)
	flags := uint16(unix.NLM_F_REQUEST)
	if ack {
		flags |= unix.NLM_F_ACK
	}
	binary.NativeEndian.PutUint16(b[6:], flags)
	binary.NativeEndian.PutUint32(b[8:], seq)
	b[16], b[17] = command, 1
	for _, a := range attrs {
		b = append(b, a...)
	}
	binary.NativeEndian.PutUint32(b, uint32(len(b)))
	return b
}

func value(attrs []byte, want uint16, size int) ([]byte, error) {
	for len(attrs) > 0 {
		if len(attrs) < 4 {
			return nil, fmt.Errorf("nl80211: truncated attribute")
		}
		n := int(binary.NativeEndian.Uint16(attrs))
		kind := binary.NativeEndian.Uint16(attrs[2:]) & 0x3fff
		if n < 4 || align4(n) > len(attrs) {
			return nil, fmt.Errorf("nl80211: invalid attribute length %d", n)
		}
		if kind == want {
			if n != 4+size {
				return nil, fmt.Errorf("nl80211: invalid attribute %d size", want)
			}
			return attrs[4:n], nil
		}
		attrs = attrs[align4(n):]
	}
	return nil, fmt.Errorf("nl80211: missing attribute %d", want)
}

func reply(b []byte, family uint16, seq uint32, ack bool) ([]byte, error) {
	msgs, err := syscall.ParseNetlinkMessage(b)
	if err != nil {
		return nil, fmt.Errorf("nl80211: parse reply: %w", err)
	}
	for _, m := range msgs {
		if m.Header.Seq != seq {
			continue
		}
		if m.Header.Type == unix.NLMSG_ERROR {
			if len(m.Data) < 4 {
				return nil, fmt.Errorf("nl80211: short acknowledgement")
			}
			code := int32(binary.NativeEndian.Uint32(m.Data))
			if code < 0 {
				return nil, fmt.Errorf("nl80211: %w", unix.Errno(-int64(code)))
			}
			if code != 0 {
				return nil, fmt.Errorf("nl80211: invalid error code %d", code)
			}
			if ack {
				return nil, nil
			}
		} else if !ack && m.Header.Type == family && len(m.Data) >= 4 {
			return m.Data[4:], nil
		}
	}
	return nil, fmt.Errorf("nl80211: unexpected reply")
}

func exchange(fd int, family uint16, command byte, seq uint32, ack bool, attrs ...[]byte) ([]byte, error) {
	if err := unix.Sendto(fd, message(family, command, seq, ack, attrs...), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("nl80211: send: %w", err)
	}
	b := make([]byte, 64<<10)
	n, from, err := unix.Recvfrom(fd, b, 0)
	if err != nil {
		return nil, fmt.Errorf("nl80211: receive: %w", err)
	}
	if peer, ok := from.(*unix.SockaddrNetlink); !ok || peer.Pid != 0 {
		return nil, fmt.Errorf("nl80211: reply is not from kernel")
	}
	return reply(b[:n], family, seq, ack)
}

func socket() (int, uint16, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_GENERIC)
	if err != nil {
		return -1, 0, err
	}
	fail := func(err error) (int, uint16, error) { unix.Close(fd); return -1, 0, err }
	tv := unix.NsecToTimeval(int64(3 * time.Second))
	for _, opt := range []int{unix.SO_RCVTIMEO, unix.SO_SNDTIMEO} {
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, opt, &tv); err != nil {
			return fail(err)
		}
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fail(err)
	}
	attrs, err := exchange(fd, genlIDCtrl, ctrlCmdGetFamily, 1, false, attribute(ctrlAttrFamilyName, []byte("nl80211\x00")))
	if err != nil {
		return fail(err)
	}
	id, err := value(attrs, ctrlAttrFamilyID, 2)
	if err != nil {
		return fail(err)
	}
	return fd, binary.NativeEndian.Uint16(id), nil
}

func powerSave(ifname string, set *bool) (bool, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return false, fmt.Errorf("nl80211: interface %s: %w", ifname, err)
	}
	fd, family, err := socket()
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	attrs := [][]byte{number(nl80211AttrIfindex, uint32(iface.Index))}
	cmd := byte(nl80211CmdGetPowerSave)
	if set != nil {
		cmd = nl80211CmdSetPowerSave
		state := uint32(0)
		if *set {
			state = 1
		}
		attrs = append(attrs, number(nl80211AttrPsState, state))
	}
	data, err := exchange(fd, family, cmd, 2, set != nil, attrs...)
	if err != nil || set != nil {
		return false, err
	}
	b, err := value(data, nl80211AttrPsState, 4)
	if err != nil {
		return false, err
	}
	state := binary.NativeEndian.Uint32(b)
	if state > 1 {
		return false, fmt.Errorf("nl80211: invalid power-save state %d", state)
	}
	return state == 1, nil
}

// GetPowerSave reports the interface's current 802.11 power-save setting.
func GetPowerSave(ifname string) (bool, error) { return powerSave(ifname, nil) }

// SetPowerSave changes only the named interface's 802.11 power-save setting.
func SetPowerSave(ifname string, enabled bool) error {
	_, err := powerSave(ifname, &enabled)
	return err
}
