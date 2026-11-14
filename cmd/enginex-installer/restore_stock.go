package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

const stockDownloadPage = "https://enginedj.com/downloads"
const stockUpdateGuide = "https://support.enginedj.com/support/solutions/articles/69000797882"
const stockReleaseURL = "https://public.inmusiccdn.com/Engine/5.3.0/RELEASE/d41cd81e67f225bf/"

// Published by Engine DJ's download page; hashes measured from those exact URLs.
// EngineX repacks retain the stock AZ01 header, so header identity alone is insufficient.
var stockFirmware = map[string]struct{ URL, SHA256 string }{
	"SC5000":       {stockReleaseURL + "SC5000-5.3.0-Update.img", "b6536da4d181d08dfa6b5c3a3b3e90cfaece889cfd7a28d013b3df1349a6c0a6"},
	"SC5000M":      {stockReleaseURL + "SC5000M-5.3.0-Update.img", "b6536da4d181d08dfa6b5c3a3b3e90cfaece889cfd7a28d013b3df1349a6c0a6"},
	"SC6000":       {stockReleaseURL + "SC6000-5.3.0-Update.img", "6d004688e643efeb1e430d06308dde255ebe77ff351fd3f168ed164af4ba57cd"},
	"SC6000M":      {stockReleaseURL + "SC6000M-5.3.0-Update.img", "6d004688e643efeb1e430d06308dde255ebe77ff351fd3f168ed164af4ba57cd"},
	"PRIMEGO":      {stockReleaseURL + "PRIMEGO-5.3.0-Update.img", "b6536da4d181d08dfa6b5c3a3b3e90cfaece889cfd7a28d013b3df1349a6c0a6"},
	"PRIME4":       {stockReleaseURL + "PRIME4-5.3.0-Update.img", "8e49313be0ef338d5452631c5772ee5b1d72e076cae65bf69a6df8b5b210029f"},
	"PRIME2":       {stockReleaseURL + "PRIME2-5.3.0-Update.img", "8e49313be0ef338d5452631c5772ee5b1d72e076cae65bf69a6df8b5b210029f"},
	"MIXSTREAMPRO": {stockReleaseURL + "MIXSTREAMPRO-5.3.0-Update.img", "8e49313be0ef338d5452631c5772ee5b1d72e076cae65bf69a6df8b5b210029f"},
}

const stockEngineXCmd = "test -f /etc/enginex/version.txt || test -f /etc/sc5000/version.txt"

var stockCacheDirs = []string{"/data/AppDataUser/AIR Music Technology/Engine/.cache/qml"}

func stockCleanupCmd() string {
	return "set -e; systemctl stop engine.service; rm -rf -- '" + strings.Join(stockCacheDirs, "' '") + "'; sync; systemctl poweroff"
}

func stockRescue() error {
	return errors.New("恢复原厂前需连接仍能启动的 EngineX 碟机以清理共享 QML 缓存。若已卡在原厂 Logo，请先用安装器刷回 EngineX，正常启动后接入同一网络，再运行 --restore-stock。没有刷写任何东西 / " +
		"Stock restore needs a running EngineX deck on the same LAN to clear its shared QML cache. If stock is already stuck at the logo, first flash EngineX with the installer, let it boot, then run --restore-stock again. Nothing flashed.")
}

func stockImage(ctx context.Context, o *options, prefix string) (string, error) {
	firmware, ok := stockFirmware[prefix]
	if !ok {
		return "", fmt.Errorf("no verified official URL for %s; download its image from %s and pass --image <file>; nothing flashed", prefix, stockDownloadPage)
	}
	path := o.Bundle
	if path == "" {
		path = filepath.Join(o.WorkDir, prefix+"-5.3.0-Update.img")
		if digest, err := fileSHA256(path); err != nil || digest != firmware.SHA256 {
			o.say("正在从 Engine DJ 官方下载 "+prefix+" 5.3.0…", "Downloading official Engine DJ 5.3.0 for "+prefix+"...")
			if err := downloadLimit(ctx, o, firmware.URL, firmware.SHA256, path, 2<<30); err != nil {
				return "", fmt.Errorf("official download: %w; download from %s and pass --image <file>; nothing flashed", err, stockDownloadPage)
			}
		}
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if digest != firmware.SHA256 {
		return "", errors.New("不是已验证的官方 Engine OS 5.3.0 镜像，拒绝 EngineX 或修改过的镜像；没有刷写任何东西 / not the verified official Engine OS 5.3.0 image; EngineX or modified images are refused; nothing flashed")
	}
	return path, nil
}

func prepareStockRestore(ctx context.Context, o *options, cl *ssh.Client, d deck, board string, m model) error {
	if _, err := sshRead(ctx, cl, stockEngineXCmd); err != nil {
		return stockRescue()
	}
	path, err := stockImage(ctx, o, m.Prefix)
	if err != nil {
		return err
	}
	if err := confirm(o, "将停止 "+m.Name+" 的 Engine，清理共享 QML 缓存并正常关机，再通过 USB 恢复官方固件。音乐和曲库不删除，请先备份。",
		"Stop Engine on "+m.Name+", clear its shared QML cache and power it off, then restore official firmware over USB. Music and libraries are kept; back up first."); err != nil {
		return err
	}
	stopCtx, cancel := context.WithTimeout(ctx, sshReadTimeout)
	defer cancel()
	if out, err := sshRun(stopCtx, cl, stockCleanupCmd(), nil); err != nil {
		return fmt.Errorf("stock restore preparation: %w\n%s", err, out)
	}
	cl.Close()
	o.Bundle = path
	d.Board = board
	return modeAB(ctx, o, d)
}

func stockUSBGuide(o *options) {
	o.say("碟机正常关机后，拔下含固件的 U 盘。SC5000/SC5000M/SC6000/SC6000M：按住 MEDIA EJECT + SOURCE + LAYER 再开机，看到 Computer Update 或芯片图标后松开，并连接 USB 线。其他一代机按官方说明操作："+stockUpdateGuide,
		"After the deck powers off, remove USB drives containing firmware. SC5000/SC5000M/SC6000/SC6000M: hold MEDIA EJECT + SOURCE + LAYER while powering on; release at Computer Update or the chip icon, then connect USB. For other 1st-gen models follow "+stockUpdateGuide)
}
