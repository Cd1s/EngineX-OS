//go:build darwin

package usb

// Keep the framework-call dependency pinned until the Darwin backend uses it.
import _ "github.com/ebitengine/purego"
