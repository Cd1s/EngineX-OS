package releasesig

// TrustedPublicKeysPEM lists the PKIX SubjectPublicKeyInfo PEM-encoded Ed25519
// public keys trusted to sign release SHA256SUMS.txt files (#239).
//
// Add the maintainer's public key generated with:
//
//	openssl genpkey -algorithm ed25519 -out /root/.config/enginex/release.key
//	openssl pkey -in /root/.config/enginex/release.key -pubout
//
// Keep both the retiring key and the new key in this slice across transition
// releases to rotate keys cleanly.
var TrustedPublicKeysPEM = []string{}
