# Security Policy

EngineX OS runs on decks that people play live on, and it updates itself over the network. We take reports about how it is built, signed and updated seriously.

## Reporting a vulnerability

Please report privately, so that users are not exposed before a fix exists:

1. Use **GitHub private vulnerability reporting**: the *Security* tab of this repository → *Report a vulnerability*.
2. If that is not available to you, open an issue that says only "security report, please contact me" and gives no details. A maintainer will start a private channel.

Include the EngineX OS version (Settings → About), the deck model, what you found, and the steps or a proof of concept. Do not include passwords, tokens or private keys; if a secret was exposed, say which kind and where.

What to expect:

- an acknowledgement within 7 days;
- a first assessment (accepted, needs information, or not a vulnerability) within 14 days;
- a fix or a mitigation plan for confirmed issues, and a release that names the fix;
- credit in the release notes if you want it.

Please give us a reasonable time (we ask for 90 days) before you publish details. We will not take action against people who report in good faith, stay within the scope below and do not harm other users' data or decks.

## Scope

In scope: the firmware images and the installer in this repository's Releases, the update path (the on-screen update, `EngineX-Installer`), the Web Console and its API on the deck, and the build and release scripts.

Out of scope: Denon DJ / inMusic firmware and Engine itself (report those to the vendor), a deck whose owner has already given an attacker SSH or physical access, social engineering, denial of service by flooding the deck's network port, and findings that need a modified or unsigned build.

## Signed releases

Every release has a `SHA256SUMS.txt` that lists all of its files, and a `SHA256SUMS.txt.sig`, an Ed25519 signature over the exact bytes of that list, made with the EngineX OS release key.

- A deck on **5.3.0.5 or later** checks the signature before it installs an update from the on-screen update page. It refuses a release that is not signed, whose signature is not valid, or whose file does not match the signed SHA-256, and says so on the screen. The signed list also names the release tag (`RELEASE-TAG.txt`), because file names carry the version but not the `-rN` revision and the release page is not signed: a genuine older revision shown under a newer tag is refused, and so is any release that is not newer than the installed one. An update is also refused, before anything is downloaded, when the deck's data volume would be left with too little free space for Engine.
- `EngineX-Installer` makes the same check before it flashes a downloaded image. A file you pass with `--image` or `--bundle` is checked when `SHA256SUMS.txt` and `SHA256SUMS.txt.sig` are in the same folder; without them it is your own build, and the installer says it cannot verify it. When GitHub cannot be read, the installer reuses a package it downloaded earlier only together with the signed list it kept at that download; it never falls back to an unsigned file by itself. The installer also refuses to install a release that is older than the one the deck already runs (naming the older file with `--image` or `--bundle` is how to do that on purpose). If it cannot read what the deck runs (a release file that is there but cannot be read), it installs nothing; a file that is not there, as in older releases without a release tag, is fine.
- Decks on **5.3.0.4 and earlier** do not check signatures. They install a release as they always did, and the signature files in a release do not disturb them. Update such a deck to 5.3.0.5 first.
- The public keys are in [`internal/releasesig/keys.go`](./internal/releasesig/keys.go), as a list, so the key can be rotated: a release that moves to a new key is signed with the old one, ships the list with both, and later releases drop the old key.

To check a download by hand, with a release's two files next to the image:

```sh
go run ./cmd/enginex-sign --verify SHA256SUMS.txt        # the signature, with the keys in this checkout
sha256sum -c --ignore-missing SHA256SUMS.txt             # the files
```

The private key never leaves the build host; it is not in this repository, in a release or in a log.

If you think the release key was exposed, tell us at once through the channel above: we will publish a release with a new key and say how to move to it.
