package config

import "gopkg.in/yaml.v3"

// Clone returns an independent copy suitable for transactional config updates.
func Clone(cfg *Config) (*Config, error) {
	if cfg == nil {
		return nil, nil
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var cloned Config
	if err = yaml.Unmarshal(data, &cloned); err != nil {
		return nil, err
	}
	cloned.Home = cfg.Home
	return &cloned, nil
}
