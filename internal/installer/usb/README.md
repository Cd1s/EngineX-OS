# Native installer USB transport

`NewBackend() (Backend, error)` selects Linux usbfs, Windows SetupAPI/CfgMgr32
with WinUSB, or macOS IOKit/CoreFoundation. Supported native targets are
Linux amd64/arm64, Windows amd64, and macOS amd64/arm64. Other targets return
an `Unsupported` error. All release targets compile with `CGO_ENABLED=0`.

The caller selects the model/protocol, attachment ID, interface number and
alternate setting. The package never downloads files, launches host commands,
installs drivers, rebinds interfaces, or selects a protocol from product text.
`Open` accepts only a vendor-specific interface (`class = 0xff`) and validates
the actual native identity. An audio, HID, or mass-storage interface is not
an update interface.

## Identity and descriptors

Enumeration uses descriptor files or registry APIs without opening/claiming
USB interfaces. IDs are scoped to one backend enumeration snapshot. A new
enumeration invalidates older unopened IDs. Open rechecks attachment identity;
replugging an identical device does not authorize reuse of an old ID.

Linux enumerates all alternates in the active configuration. Darwin discovers
the registry's current interface/alternate and obtains endpoint descriptors
after opening the selected interface. Windows discovers hardware IDs, bound
service and registered paths (including the U-Boot, Android and generic USB
GUIDs); its enumeration does not open WinUSB merely to obtain descriptors.
Windows enumeration can therefore return an empty `Interfaces` list. The
caller must know which interface it intends to open, then use `Device.Info()`
for the verified interface and endpoints. A generic USB path or `Driver` text
alone never establishes a working WinUSB binding.

After Open, `Info().Interfaces` contains the claimed interface and endpoints.
Returned descriptor slices are copies. Each call validates endpoint ownership,
bulk transfer type and direction. Interface/endpoint control recipients cannot
escape the claimed interface; address/configuration/alternate changes belong
to Open rather than protocol control traffic.

## Transfer and cancellation contract

Each `WriteBulk` is one OS transfer, including `nil`/empty-buffer writes. The
Windows backend disables implicit short-packet termination so packet-aligned
payloads do not acquire an extra ZLP. The protocol layer owns chunking and any
explicit final ZLP. Individual bulk calls are limited to 16 MiB; control data
is limited to 65535 bytes. Short results are returned without replay, and the
caller must validate them.

An explicit context deadline is honored, including deadlines longer than the
30-second default. The default applies only when the caller has no deadline.
Transfers are serialized per handle. Waiting for that handle is cancellable.
Close cancels an active transfer, waits for cleanup, then releases resources;
Close is idempotent.

Linux pins URB/data memory until usbfs reaps the request; an unrecoverable reap
error closes the usbfs handle before unpinning. Windows pins overlapped I/O
and its buffers, uses `CancelIoEx`, joins the cancellation callback and drains
`WinUsb_GetOverlappedResult`. Darwin pins native buffers until synchronous
IOKit methods return and joins its AbortPipe watcher before releasing COM.

Errors preserve native/context causes through `Unwrap()`. `Kind` distinguishes
permission, binding, busy, disconnection, timeout, cancellation, protocol and
unsupported-target failures. `MayHaveWritten` means an OUT request may have
reached the device even if the returned length is zero. Never automatically
retry such an operation.

## Verification and qualification limits

The package tests cover malformed descriptors, interface/endpoint isolation,
stale and identical attachments, short transfers, explicit ZLP submission,
deadline/cancellation handling, native buffer ownership and handle cleanup.
Platform call injection proves software behavior; it does not prove a USB
packet was transmitted. `TestNativeEnumeration` never opens a device.

Darwin ABI tests call actual CoreFoundation functions with a 16-byte UUID
passed and returned by value. The native QueryInterface test requires
`ENGINEX_USB_TEST_REGISTRY_ID` set to a PM-reserved vendor update interface;
it is skipped without that fixture. Do not substitute an unrelated in-use
storage/audio/HID interface. This test queries/releases a userclient without
claiming the interface; actual open/close and transfers are separate checks.

Still required for hardware qualification:

* Native Linux, Windows and macOS update-interface open/close, bulk/control
  transfers, short responses, unplug/cancel and observed final ZLP.
* Clean Windows automatic WinUSB binding for each relevant device family.
  No INF, driver installer, test-signing change or automatic rebinding is
  implemented. Unsupported binding remains a release qualification blocker.
* Actual Darwin USB userclient creation, QueryInterface, claim and transfers
  on both CPU architectures. Rosetta execution is not native Intel evidence.
* Each model/protocol family's physical qualification, scheduled separately
  by the PM. The transport package's tests do not qualify flashing or reboot.

Run compilation and Go suites on an approved ARM build host. Linux tests run
in a private mount namespace with a private tmpfs `/data`; preserve the host
`/tmp` mode. Cross-build package tests for Windows/macOS and copy only those
executables to the native runtime host. Do not compile or install dependencies
there. Keep host aliases, fixture digests and exact revision results in the
external task evidence, not device serials or private network details.
