package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Cd1s/EngineX-OS/internal/installer/proto"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/azupdate"
	"github.com/Cd1s/EngineX-OS/internal/installer/proto/fastboot"
	"github.com/Cd1s/EngineX-OS/internal/installer/usb"
)

type updateSession struct {
	device usb.Device
	fb     *fastboot.Client
	az     *azupdate.Client
	board  string
}

func (o *options) enumerateUSB(ctx context.Context) ([]usb.DeviceInfo, error) {
	if o.USB == nil {
		var err error
		o.USB, err = usb.NewBackend()
		if err != nil {
			return nil, err
		}
	}
	return o.USB.Enumerate(ctx)
}

func usbLine(info usb.DeviceInfo) string {
	return fmt.Sprintf("%04x:%04x %s", info.VID, info.PID, info.Product)
}

// tryUSB claims only a single supported update interface. RockUSB remains diagnostic.
func tryUSB(ctx context.Context, o *options, board string) (found bool, err error) {
	devices, err := o.enumerateUSB(ctx)
	if err != nil {
		return false, err
	}
	type candidate struct {
		info usb.DeviceInfo
		it   usb.Interface
		az   bool
	}
	var candidates []candidate
	for _, info := range devices {
		id := fmt.Sprintf("%04x:%04x", info.VID, info.PID)
		az := id == azID
		if !az && gen1USB[id] == "" && gen2USB[id] == "" && id != "18d1:4ee0" {
			continue
		}
		interfaces := info.Interfaces
		if len(interfaces) == 0 {
			if !strings.EqualFold(info.Driver, "WinUSB") {
				continue
			}
			// WinUSB reports descriptors on Open; verify interface 0 there.
			interfaces = []usb.Interface{{}}
		}
		for _, it := range interfaces {
			if len(info.Interfaces) != 0 && !updateInterface(it, az) {
				continue
			}
			candidates = append(candidates, candidate{info: info, it: it, az: az})
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}
	if len(candidates) != 1 {
		return true, errors.New("multiple deck update interfaces found; connect only the deck to install")
	}
	c := candidates[0]
	dev, err := o.USB.Open(ctx, c.info.ID, c.it.Number, c.it.Alternate)
	if err != nil {
		var detail *usb.Error
		if errors.As(err, &detail) && detail.Kind == usb.Permission && goos == "linux" && geteuid() != 0 {
			o.hint("permission", "没有 USB 访问权限，请用 sudo 运行本工具。", "USB access denied; run this tool with sudo.")
		}
		return false, err
	}
	defer func() { err = errors.Join(err, dev.Close()) }()
	s := &updateSession{device: dev}
	var eps proto.Endpoints
	for _, it := range dev.Info().Interfaces {
		if it.Number != c.it.Number || it.Alternate != c.it.Alternate || !updateInterface(it, c.az) {
			continue
		}
		for _, ep := range it.Endpoints {
			if ep.Attributes&3 != 2 {
				continue
			}
			if ep.Address&0x80 != 0 {
				if eps.In != 0 {
					return false, errors.New("ambiguous update bulk IN endpoint")
				}
				eps.In = ep.Address
			} else {
				if eps.Out != 0 {
					return false, errors.New("ambiguous update bulk OUT endpoint")
				}
				eps.Out, eps.MaxPacketSize = ep.Address, ep.MaxPacketSize
			}
		}
	}
	if c.az {
		if o.RestoreStock {
			return true, errors.New("2nd-gen stock restore uses LAN/SSH --uninstall; nothing flashed")
		}
		s.az, err = azupdate.New(dev, eps)
		if err != nil {
			return false, err
		}
		return true, modeAZ(ctx, o, s, board, usbLine(dev.Info()))
	}
	s.fb, err = fastboot.New(dev, eps, fastboot.WithOnInfo(func(info string) { fmt.Fprintln(o.Out, info) }))
	if err != nil {
		return false, err
	}
	product, err := s.fb.GetVar(ctx, "product")
	if err != nil {
		fallback, ok := proto.Gen1BoardByUSBID(c.info.VID, c.info.PID)
		if !errors.Is(err, fastboot.ErrUnsupportedVar) || !ok {
			return true, fmt.Errorf("cannot read the fastboot product: %w", err)
		}
		product = fallback.Code
	}
	s.board = boardCode(product)
	if _, _, known := lookupBoard(board); known && boardCode(board) != s.board {
		return true, errors.New("LAN board and selected USB deck disagree; nothing flashed")
	}
	return true, modeB(ctx, o, s)
}

func updateInterface(it usb.Interface, az bool) bool {
	if az {
		return it.Class == 0xff && it.Subclass == 0x61 && it.Protocol == 0x7a
	}
	return it.Class == 0xff && it.Subclass == 0x42 && it.Protocol == 3
}

func transferProgress(o *options) proto.ProgressFunc {
	lastPhase, lastPercent := "", -1
	return func(p proto.Progress) {
		percent := int(p.Sent * 100 / p.Total)
		if p.Phase != lastPhase || percent != lastPercent {
			fmt.Fprintf(o.Out, "%s: %d/%d bytes (%d%%)\n", p.Phase, p.Sent, p.Total, percent)
			lastPhase, lastPercent = p.Phase, percent
		}
	}
}

func flashFile(ctx context.Context, o *options, s *updateSession, partition, path string, maxDown int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	info := s.device.Info()
	fallback, _ := proto.Gen1BoardByUSBID(info.VID, info.PID)
	return s.fb.Flash(ctx, partition, proto.Image{Data: f, Size: st.Size()}, fastboot.FlashOptions{
		ExpectedBoard: s.board, AllowMissingProductBoard: fallback.Code,
		MaxDownloadSize: maxDown, AllowSparse: true, SparseBlockSize: 512,
	}, transferProgress(o))
}

func usbFlash(ctx context.Context, o *options, s *updateSession, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = s.az.WriteData(ctx, proto.Image{Data: f, Size: st.Size()}, transferProgress(o))
	return err
}
