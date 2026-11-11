# USB-cable link (adb-like) for first-generation decks: research note

Status: research only, nothing implemented. Target: EngineX 5.3.0.5 on the official 5.3.0 base. Question: can a developer reach a deck over the USB-B "computer" cable (SSH, deck-push) the way `adb` reaches a phone, without changing what the stock firmware does on that port?

Everything here was read from the official `SC5000-5.3.0-Update.img`: its rootfs, its device trees, and the kernel config that the shipped kernel embeds (`CONFIG_IKCONFIG=y`). Nothing was run on a deck. Lines marked **[deck check]** still need a read-only look on a real deck.

## 1. Hardware and device tree

The 5.3.0 rootfs ships `/boot/zImage-6.18.57-az01-2026-10-15-rt6` and one device tree per board (`rk3288-az01-jp07.dtb`, `-jp07-c`, `-jp08*`, `-jp11*`). The RK3288 has five USB controllers; in `rk3288-az01-jp07.dtb` they are:

| Node | Controller | State |
| --- | --- | --- |
| `usb@ff500000` | EHCI host | `okay` |
| `usb@ff520000` | OHCI host | `disabled` |
| `usb@ff540000` | DWC2, `dr_mode = "host"` | `okay` |
| `usb@ff580000` | DWC2 OTG, `dr_mode = "otg"` | `okay` |
| `usb@ff5c0000` | HSIC EHCI | `disabled` |

Only `usb@ff580000` can act as a USB device, so it has to be the USB-B port. The kernel builds DWC2 as dual-role (`CONFIG_USB_DWC2_DUAL_ROLE=y`), so at runtime this controller is the deck's only UDC. **[deck check]** `ls /sys/class/udc` should list `ff580000.usb`, and `cat /sys/class/udc/*/state` should change when a computer is plugged in.

## 2. What the stock firmware does with the USB-B port

1. **Computer Mode (runtime, Engine).** The Control Center tile `Skins/Embedded/Views/ControlCenter/Tabs/ControlCenter/Entries/ComputerMode.qml` ("Computer Mode") opens `PlanckGUI.views.storageMode`. Engine's `UsbGadgetControl` (`DevicesController::setupUsbGadgetControl`, `enableMassStorageAccess`, `disableMassStorageAccess`, `onUdcStateChanged`, `startUdcMonitoring`) runs `/usr/Engine/Scripts/setup-usb-gadget.sh` with `--usb-vid`, `--usb-pid`, `--manufacturer-name`, `--product-name`, `--enable-ms` and `--usb-ids` (the attached drives) and `create`. Later it runs `deleteMassStorage` or `rebindGadget`, and it watches `/sys/class/udc/<udc>/state`. The computer then sees the deck's drives as USB mass storage. These are the only gadget arguments that occur as strings in the Engine binary.
2. **`setup-usb-gadget.sh`** (498 lines) builds one configfs gadget under `/sys/kernel/config/usb_gadget/<name>` with configuration `c.1`. It takes the first UDC (`ls /sys/class/udc | head -n 1`) and writes it to `<gadget>/UDC`. It can also add MIDI (`--enable-midi`), HID (`--enable-hid`) and network (`--enable-net`, one `ncm.usb0` function), but Engine never passes those flags. `delete` unbinds the gadget and removes all of its functions. `sys-kernel-config.mount` mounts configfs.
3. **Computer Update Mode** (firmware update over the cable) is not Linux userspace: it runs from the boot chain and az01-update, see `docs/manual/03-features-and-faq.md` and `docs/GEN2-USB-UPDATE-MODE.md`. A switch in the running system does not affect it.

In normal use nothing is bound to the UDC until the user taps Computer Mode. **[deck check]** `cat /sys/kernel/config/usb_gadget/*/UDC` while idle and while Computer Mode is on.

## 3. Kernel config of the 5.3.0 base

The full config is in [`docs/re-report/ENGINE-5.3.0-kernel.config`](../re-report/ENGINE-5.3.0-kernel.config). It was taken from the `IKCFG_ST` block of the decompressed `zImage-6.18.57-az01-2026-10-15-rt6` (Linux 6.18.57, PREEMPT_RT, Yocto GCC 13.4.0). Compared with the earlier bases already in `docs/re-report`:

| Option | 4.3.4 (6.1.111) | 5.0.4 (6.6.119) | 5.3.0 (6.18.57) |
| --- | --- | --- | --- |
| `USB_GADGET`, `USB_LIBCOMPOSITE`, `USB_CONFIGFS` | y | y | y |
| `USB_DWC2_DUAL_ROLE` (not `_PERIPHERAL`) | y | y | y |
| `USB_CONFIGFS_NCM`, `_ECM`, `_ECM_SUBSET`, `_EEM`, `_RNDIS` | y | y | y |
| `USB_CONFIGFS_MASS_STORAGE`, `_F_FS`, `_F_UAC2`, `_F_MIDI`, `_F_HID`, `_ACM` | y | y | y |
| `USB_ROLE_SWITCH`, `PREEMPT_RT` | y | y | y |
| legacy `USB_ETH` (`g_ether`), `USB_G_NCM` | n | n | n |

Every network gadget function is built in, so no module and no kernel change is needed. Only configfs gadgets work; the legacy `g_ether` module does not exist. **[deck check]** `zcat /proc/config.gz | grep USB_CONFIGFS_` on the running deck.

Network management on 5.3.0: `systemd-networkd` is enabled, but its only network file, `69-default.network`, sets `Unmanaged=yes` for every link. `connman` manages wired and Wi-Fi (`PreferredTechnologies=wifi,ethernet`), and its binary contains the `gadget` technology. That technology claims USB gadget network interfaces for tethering.

## 4. How an opt-in developer switch could add a network link

Goal: off by default and invisible to DJs. When a developer turns it on, the computer sees an ECM/NCM (or RNDIS) network interface on the USB-B cable. Computer Mode and Computer Update Mode must work exactly as before, and no stock script, unit or connman file may change.

1. **Its own gadget, never the stock one.** Create `/sys/kernel/config/usb_gadget/enginex-dev` with its own strings and a USB ID that is not one of inMusic's. Add one network function: `ncm.dev0` (Windows 10/11, Linux and recent macOS have inbox NCM drivers) or `ecm.dev0` (macOS and Linux), plus `rndis.dev0` with Microsoft OS descriptors only if older Windows must work. Set the function's `ifname` to `devusb%d` so the interface name never collides with `usb0`, which the stock `--enable-net` path would use. Do not modify the stock gadget or its functions.
2. **One UDC, so the stock gadget always wins.** A UDC binds one gadget at a time, and `setup-usb-gadget.sh` writes the UDC without checking whether another gadget holds it. The switch must therefore let go before Engine needs the port:
   - bind `enginex-dev` only while no other gadget in configfs has a non-empty `UDC`;
   - before Engine enters Computer Mode, write an empty string to `enginex-dev/UDC`. The clean place is our own preloaded UI hook: wrap `UsbGadgetControl::enableMassStorageAccess` so it unbinds `enginex-dev` first, then calls the stock code. That changes neither the script nor the binary on disk. Inotify on configfs alone would race with the script's `echo $UDC > UDC`;
   - rebind after `disableMassStorageAccess`, or once every stock gadget's `UDC` is empty again.
3. **Address without DHCP.** Give `devusb0` a fixed link-local address on the deck (`169.254.x.y/16`, constant per deck). The computer picks its own link-local address. No DHCP server, no route and no DNS change on the deck. EngineX `sshd` already listens on every interface with key-only authentication, so `ssh root@<link-local>` works, and deck-push can use it through a `decks.ini` alias.
4. **Keep connman away.** The `gadget` technology could claim the interface and start tethering. Before shipping, confirm on a deck that connman leaves `devusb*` alone. The interface name alone may not be enough, because connman also checks the driver type. If it does not leave it alone, set the interface up from the switch's service after connman has enumerated it, with `connmanctl disable gadget` scoped to that service. Never edit `/etc/connman/main.conf`. **[deck check]**
5. **Where it lives.** A `settings.conf` key (for example `usbLink=1`, default off) toggled from the web console's developer page, and one small `enginex-usblink.service` with `Wants=`/`After=` on `sys-kernel-config.mount` only. Do not use `Requires=` on a vendor unit, see CONTRIBUTING. The service owns `enginex-dev` and removes it when stopped.
6. **Real-time audio.** Gadget traffic raises the DWC2 OTG interrupt. `sc5000-perf-tune` already moves every interrupt that is not audio off the audio CPU (CPU 0, CPU 3 takes the peripherals). Check that the OTG IRQ really lands there: watch `cat /proc/interrupts` while a large file copies over the link during playback.

## 5. Open questions before implementing (5.3.0.5)

- [deck check] the UDC name and its `state` with and without a computer; whether anything binds the UDC at boot.
- [deck check] whether connman's gadget technology claims `devusb0`.
- Whether Engine's `onUdcStateChanged` reacts badly when a different gadget is bound (for example, it shows "computer connected" for our link). Test this in a sandbox by preloading a fake `/sys/class/udc` before going to a deck.
- Which host drivers to support: NCM for everything current, or also RNDIS for older Windows.
- Second-generation decks (AZ02/AZ04) have other controllers and their own update gadget (`docs/GEN2-USB-UPDATE-MODE.md`). This note covers the first generation only.
