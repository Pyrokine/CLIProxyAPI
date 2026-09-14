package config

// HomeConfig stores runtime-only Home control plane settings from -home-jwt.
type HomeConfig struct {
	Enabled                 bool          `yaml:"enabled" json:"enabled"`
	NodeID                  string        `yaml:"-" json:"-"`
	Host                    string        `yaml:"host" json:"-"`
	Port                    int           `yaml:"port" json:"-"`
	DisableClusterDiscovery bool          `yaml:"disable-cluster-discovery" json:"-"`
	TLS                     HomeTLSConfig `yaml:"tls" json:"-"`
}

// HomeTLSConfig configures client-side TLS for the home Redis connection.
type HomeTLSConfig struct {
	Enable              bool   `yaml:"enable" json:"-"`
	ServerName          string `yaml:"server-name" json:"-"`
	InsecureSkipVerify  bool   `yaml:"insecure-skip-verify" json:"-"`
	CACert              string `yaml:"ca-cert" json:"-"`
	ClientCert          string `yaml:"-" json:"-"`
	ClientKey           string `yaml:"-" json:"-"`
	UseTargetServerName bool   `yaml:"-" json:"-"`
}

// ApplyHomeRuntimeConfig disables local entry points that Home replaces at runtime.
func ApplyHomeRuntimeConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	cfg.APIKeys = nil
	cfg.UsageStatisticsEnabled = true
	cfg.DisableCooling = true
	cfg.SaveCooldownStatus = false
	cfg.WebsocketAuth = false
	cfg.RemoteManagement.AllowRemote = false
	cfg.RemoteManagement.DisableControlPanel = true
}
