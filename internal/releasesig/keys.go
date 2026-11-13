package releasesig

// TrustedPublicKeysPEM lists the PKIX SubjectPublicKeyInfo PEM-encoded Ed25519
// public keys trusted to sign release SHA256SUMS.txt files (#239). Decks (from
// 5.3.0.5 on) and the installer verify every release against this list, so a
// release is signed with any one of these keys.
//
// The release key is made once, on the release build host, by the maintainer:
//
//	umask 077 && openssl genpkey -algorithm ed25519 -out /root/.config/enginex/release.key
//	openssl pkey -in /root/.config/enginex/release.key -pubout
//
// Paste the output of the second command (the public key only, never the private
// key) as a raw string entry, one key per entry, keeping the PEM lines unindented:
//
//	var TrustedPublicKeysPEM = []string{
//		`-----BEGIN PUBLIC KEY-----
//	MCowBQYDK2VwAyEA...
//	-----END PUBLIC KEY-----
//	`,
//	}
//
// With the list empty a build trusts nothing: every update is refused, and
// build-release.sh, sign-release.sh and the release gate stop. Keep the retiring
// key and the new key in this slice across the transition release(s) to rotate
// keys cleanly (see SECURITY.md and .claude/rules/release-workflow.md).
var TrustedPublicKeysPEM = []string{}
