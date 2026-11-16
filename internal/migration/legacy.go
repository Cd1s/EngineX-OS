// Package migration contains only contracts read from releases before the EngineX rename.
package migration

const (
	// Saved playlist IDs from releases before the rename.
	StemsListPrefix = "sc5000-stems-"
	// Older renderer credentials, read only when the current file is missing.
	StemsTokenFile = "/etc/sc5000-stems/token"
	// Read-only fixture path on the build hosts.
	LabDir = "/root/build/sc5000-lab"
	// Legacy release reader or rollback contract.
	DataDir = "/data/sc5000-custom"
	// Legacy release reader or rollback contract.
	DataRelative = "data/sc5000-custom"
	// Legacy release reader or rollback contract.
	WorkerPath = "/opt/sc5000-stems/worker.py"
	// Legacy release reader or rollback contract.
	ImageDir = "/etc/sc5000"
	// Legacy release reader or rollback contract.
	MediaCache = "/media/sc5000-sd-cache"
	// Legacy release reader or rollback contract.
	BroadcastInput = "/tmp/sc5000-broadcast.pcm"
	// Legacy release reader or rollback contract.
	TailscaleSocket = "/run/sc5000-tailscaled.sock"
	// Legacy release reader or rollback contract.
	DebugFlag = "/tmp/sc5000-debug"
	// Pending removal: API alias used by installed clients.
	APIPrefix = "/v1/sc5000"
	// Legacy release reader or rollback contract.
	SuitePattern = "[s]c5000-suite"
	// Legacy release reader or rollback contract.
	AlsaFile = "99-sc5000-custom.conf"
	// Legacy release reader or rollback contract.
	StemsEnvPrefix = "SC5000_STEMS_"
	// Legacy release reader or rollback contract.
	CustomEnv = "SC5000_CUSTOM"
	// Legacy release reader or rollback contract.
	AlsaEnv = "SC5000_ALSA_OVERLAY"
	// Legacy release reader or rollback contract.
	ScaleEnv = "SC5000_UI_SCALE"
	// Legacy release reader or rollback contract.
	DataName = "sc5000-custom"
	// Legacy release reader or rollback contract.
	Suite = "sc5000-suite"
	// Legacy release reader or rollback contract.
	Deemix = "sc5000-deemixd"
	// Legacy release reader or rollback contract.
	TailscaleUnit = "sc5000-tailscaled"
	// Legacy release reader or rollback contract.
	Token = "sc5000-token"
	// Legacy release reader or rollback contract.
	RefreshToken = "sc5000-refresh"
	// Legacy release reader or rollback contract.
	PlaceholderHost = "sc5000.lan"
	// Legacy release reader or rollback contract.
	Stemsd = "sc5000-stemsd"
	// Legacy release reader or rollback contract.
	ProjectName = "sc5000"
)
