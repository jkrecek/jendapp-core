// Package config loads the server's own TOML config.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Listen address. Bind to the Tailscale IP (or 127.0.0.1) — never 0.0.0.0.
	Listen  string `toml:"listen"`
	DataDir string `toml:"data_dir"`
	// URL the apps use (e.g. the `tailscale serve` HTTPS address); printed by `pair`.
	PublicURL string `toml:"public_url"`
	// Claude Code CLI binary; resolved via PATH when not absolute.
	ClaudeBin string `toml:"claude_bin"`
	// Max concurrent Claude processes; the rest queue.
	MaxJobs int `toml:"max_jobs"`
	// Module directories, each containing a jendapp.toml.
	Modules []string `toml:"modules"`
}

func Load(path string) (*Config, error) {
	c := &Config{
		Listen:    "127.0.0.1:8787",
		DataDir:   "~/.jendapp",
		ClaudeBin: "claude",
		MaxJobs:   2,
	}
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.DataDir = ExpandHome(c.DataDir)
	for i, m := range c.Modules {
		c.Modules[i] = ExpandHome(m)
	}
	if c.MaxJobs < 1 {
		c.MaxJobs = 1
	}
	return c, nil
}

func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
