package config

import (
	"testing"
	"time"
)

func TestDefault_fallbacks(t *testing.T) {
	// Empty env → built-in defaults.
	for _, k := range []string{"BOOKWATCH_USER_AGENT", "BOOKWATCH_TIMEOUT", "BOOKWATCH_PORT", "BOOKWATCH_PASSWORD"} {
		t.Setenv(k, "")
	}
	c := Default()
	if c.UserAgent == "" {
		t.Error("user agent default is empty")
	}
	if c.Timeout != 30*time.Second {
		t.Errorf("timeout default: %v", c.Timeout)
	}
	if c.Port != "8080" {
		t.Errorf("port default: %q", c.Port)
	}
}

func TestDefault_envOverrides(t *testing.T) {
	t.Setenv("BOOKWATCH_PORT", "9999")
	t.Setenv("BOOKWATCH_TIMEOUT", "5")
	t.Setenv("BOOKWATCH_PASSWORD", "pw")
	c := Default()
	if c.Port != "9999" {
		t.Errorf("port: %q", c.Port)
	}
	if c.Timeout != 5*time.Second {
		t.Errorf("timeout: %v", c.Timeout)
	}
	if c.Password != "pw" {
		t.Errorf("password: %q", c.Password)
	}
}

func TestEnvInt_invalidFallsBack(t *testing.T) {
	t.Setenv("BOOKWATCH_TIMEOUT", "notanumber")
	if c := Default(); c.Timeout != 30*time.Second {
		t.Errorf("invalid int should fall back to 30s, got %v", c.Timeout)
	}
}
