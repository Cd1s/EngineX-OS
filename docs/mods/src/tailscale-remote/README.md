# Tailscale Remote 1.104.5

Official MIT-licensed status/diagnostic Mod for the existing EngineX OS
[Tailscale integration (#130)](https://github.com/Cd1s/EngineX-OS/issues/130).

`sh tailscale-helper.sh status` checks and calls
`/data/enginex/tailscale/bin/tailscale --socket=/run/sc5000-tailscaled.sock status`;
`json` requests structured status. A missing binary returns a diagnostic and exit
code 3. Loader start/stop actions are intentionally no-ops: enabling this status
card never starts a VPN or changes service exposure.

The QML card uses the existing system API at
`http://127.0.0.1:8080/v1/sc5000/tailscale` and links to the Web Console's Remote
access controls. Host overlay component:
`EngineXMods/tailscale-remote/Panel.qml`. The host UI must load this component.
If authentication is required, use the Web Console; the card stores no token.

The package does **not** contain a Tailscale binary. Install/sign in explicitly
using Web Console → Remote access, which verifies the separate release assets:
[Tailscale v1.104.5](https://github.com/Cd1s/EngineX-OS/releases/tag/tailscale-v1.104.5).
Asset base URL:
`https://github.com/Cd1s/EngineX-OS/releases/download/tailscale-v1.104.5/`.
Userspace networking requires no TUN kernel module. Real deck CPU/RAM headroom,
all-model behavior and control-server combinations remain hardware-dependent;
this package adds no new hardware-validation claim.
