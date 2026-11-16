// Package migration contains only contracts read from releases before the EngineX rename.
package migration

const (
	// Saved playlist IDs from releases before the rename.
	StemsListPrefix = "sc5000-stems-"
	// Older renderer credentials, read only when the current file is missing.
	StemsTokenFile = "/etc/sc5000-stems/token"
	// Read-only fixture path on the build hosts.
	LabDir = "/root/build/sc5000-lab"
	// Pre-5.3.0.5 upgrade input.
	DataDir = "/data/sc5000-custom"
	// Pre-5.3.0.5 upgrade input.
	DataRelative = "data/sc5000-custom"
	// Pre-5.3.0.5 upgrade input.
	WorkerPath = "/opt/sc5000-stems/worker.py"
	// Pre-5.3.0.5 upgrade input.
	ImageDir = "/etc/sc5000"
	// Pre-5.3.0.5 upgrade input.
	MediaCache = "/media/sc5000-sd-cache"
	// Pre-5.3.0.5 upgrade input.
	BroadcastInput = "/tmp/sc5000-broadcast.pcm"
	// Pre-5.3.0.5 upgrade input.
	TailscaleSocket = "/run/sc5000-tailscaled.sock"
	// Pre-5.3.0.5 upgrade input.
	DebugFlag = "/tmp/sc5000-debug"
	// Pre-5.3.0.5 device detection and retired-namespace rejection.
	APIPrefix = "/v1/sc5000"
	// Pre-5.3.0.5 upgrade input.
	SuitePattern = "[s]c5000-suite"
	// Pre-5.3.0.5 upgrade input.
	AlsaFile = "99-sc5000-custom.conf"
	// Pre-5.3.0.5 upgrade input.
	StemsEnvPrefix = "SC5000_STEMS_"
	// Pre-5.3.0.5 upgrade input.
	CustomEnv = "SC5000_CUSTOM"
	// Pre-5.3.0.5 upgrade input.
	AlsaEnv = "SC5000_ALSA_OVERLAY"
	// Pre-5.3.0.5 upgrade input.
	ScaleEnv = "SC5000_UI_SCALE"
	// Pre-5.3.0.5 upgrade input.
	DataName = "sc5000-custom"
	// Pre-5.3.0.5 upgrade input.
	Suite = "sc5000-suite"
	// Pre-5.3.0.5 upgrade input.
	Deemix = "sc5000-deemixd"
	// Pre-5.3.0.5 upgrade input.
	TailscaleUnit = "sc5000-tailscaled"
	// Pre-5.3.0.5 upgrade input.
	Token = "sc5000-token"
	// Pre-5.3.0.5 upgrade input.
	RefreshToken = "sc5000-refresh"
	// Pre-5.3.0.5 upgrade input.
	PlaceholderHost = "sc5000.lan"
	// Pre-5.3.0.5 upgrade input.
	ProjectName = "sc5000"
)
