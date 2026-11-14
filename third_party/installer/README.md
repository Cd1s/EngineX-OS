# Installer dependency notices

The native installer uses these pinned Go modules. They are linked into the
executable; users do not install them separately.

| Module | Version | License | Minimum Go version |
| --- | --- | --- | --- |
| `github.com/ulikunitz/xz` | `v0.5.17` | BSD-3-Clause | 1.20 |
| `github.com/ebitengine/purego` | `v0.11.3` | Apache-2.0, with Go-derived BSD-3-Clause source | 1.25.0 |

The project requires Go 1.26.0 or newer, which satisfies both requirements.
`purego` is imported only by Darwin-tagged installer source. XZ supports the
AZ01 rootfs reader on every installer platform.

The module licenses are copied verbatim from the pinned module archives into
`xz/LICENSE` and `purego/LICENSE`. `GO-LICENSE` retains the Go Authors' notice
for Go-derived source, including purego's `internal/fakecgo`. No upstream
implementation source is modified or vendored here. No upstream `NOTICE` file
was present in either pinned module.

Both licenses permit redistribution with this project's MIT-licensed code;
their own terms and attribution requirements still apply. Preserve these
license files in the installer archives when the native packages are integrated.
The XZ command-line program and its code are not part of the installer.
