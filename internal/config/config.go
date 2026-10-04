// Package config holds the few runtime settings Invoicer reads from the
// environment. Everything is optional: a bare `docker run` works.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Version is injected at build time with -ldflags "-X ...config.Version=v1.2.3".
var Version = "v0.0.0-dev"

// Repo is the GitHub repository used for self-updates.
var Repo = "flocom/Invoicer"

type Config struct {
	DataDir  string // persistent volume (database, keys, backups, updated binaries)
	HTTPAddr string // plain HTTP listener
	// TLS = "auto" enables built-in Let's Encrypt certificates (ports 8080 + 8443
	// inside the container, map them to 80 and 443).
	TLS       string
	HTTPSAddr string
	// MasterKey optionally overrides the auto-generated encryption key file.
	MasterKey string
	// AutoUpdate can be set to "false" to disable automatic installs (checks still run).
	AutoUpdate bool
}

func Load() Config {
	c := Config{
		DataDir:    env("INVOICER_DATA", "/data"),
		HTTPAddr:   env("INVOICER_HTTP_ADDR", ":8080"),
		HTTPSAddr:  env("INVOICER_HTTPS_ADDR", ":8443"),
		TLS:        strings.ToLower(env("TLS", "")),
		MasterKey:  os.Getenv("INVOICER_MASTER_KEY"),
		AutoUpdate: strings.ToLower(env("AUTO_UPDATE", "true")) != "false",
	}
	if p := os.Getenv("PORT"); p != "" && os.Getenv("INVOICER_HTTP_ADDR") == "" {
		c.HTTPAddr = ":" + p
	}
	return c
}

func (c Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.DataDir}, parts...)...)
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
