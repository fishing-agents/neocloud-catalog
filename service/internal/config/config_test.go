package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLayersOverlayThenEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10-db.toml", "[database]\nmax_conns = 5\ndsn = \"postgres://overlay\"\n")
	write("20-db.toml", "[database]\nmax_conns = 7\n")
	t.Setenv("NEOCLOUD_CATALOG_DATABASE_MAX_CONNS", "9")
	t.Setenv("NEOCLOUD_TELEMETRY_METRICS_ADDR", "127.0.0.1:1") // neocloud-api's prefix, not ours

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.DSN != "postgres://overlay" || cfg.Database.MaxConns != 9 {
		t.Errorf("database = %+v, want overlay dsn and env max_conns 9", cfg.Database)
	}
	if cfg.Telemetry.MetricsAddr != "0.0.0.0:8081" {
		t.Errorf("metrics_addr = %q, want the default", cfg.Telemetry.MetricsAddr)
	}
}

func TestLoadRefusesInvalidConfig(t *testing.T) {
	cases := map[string]struct{ env, value, want string }{
		"no dsn":              {"NEOCLOUD_CATALOG_DATABASE_DSN", "", "database.dsn"},
		"metrics on api port": {"NEOCLOUD_CATALOG_TELEMETRY_METRICS_ADDR", "0.0.0.0:8080", "telemetry.metrics_addr"},
		"zero timeout":        {"NEOCLOUD_CATALOG_SERVER_READ_TIMEOUT", "0s", "server.read_timeout"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(c.env, c.value)
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Load = %v, want an error naming %s", err, c.want)
			}
		})
	}
}
