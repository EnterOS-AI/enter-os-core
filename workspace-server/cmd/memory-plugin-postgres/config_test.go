package main

import (
	"strings"
	"testing"
)

// TestLoadConfig_DefaultListenAddrIsLoopback pins the default-bind contract.
//
// Why this matters: with the prior `:9100` default, the plugin listened on
// every interface. Inside the container it didn't matter (no host port
// mapping today), but a future change that publishes 9100 OR a cross-host
// sidecar deploy would have exposed an unauth'd memory store. Loopback by
// default is the least-privilege baseline; operators with a multi-host
// topology override via MEMORY_PLUGIN_LISTEN_ADDR.
func TestLoadConfig_DefaultListenAddrIsLoopback(t *testing.T) {
	t.Setenv("MEMORY_PLUGIN_DATABASE_URL", "postgres://stub")
	t.Setenv("MEMORY_PLUGIN_LISTEN_ADDR", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !strings.HasPrefix(cfg.ListenAddr, "127.0.0.1:") {
		t.Errorf("default ListenAddr must bind loopback-only, got %q "+
			"(security regression — would expose plugin on every interface)",
			cfg.ListenAddr)
	}
}

func TestLoadConfig_ListenAddrEnvOverride(t *testing.T) {
	t.Setenv("MEMORY_PLUGIN_DATABASE_URL", "postgres://stub")
	t.Setenv("MEMORY_PLUGIN_LISTEN_ADDR", ":9100")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ListenAddr != ":9100" {
		t.Errorf("env override ignored: want :9100, got %q", cfg.ListenAddr)
	}
}

func TestLoadConfig_MissingDatabaseURL(t *testing.T) {
	t.Setenv("MEMORY_PLUGIN_DATABASE_URL", "")

	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig must error when MEMORY_PLUGIN_DATABASE_URL is empty")
	}
}
