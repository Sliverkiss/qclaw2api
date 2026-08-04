package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDefault 校验默认值符合 SPEC §2.9。
func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7865" {
		t.Errorf("Listen = %q, want :7865", c.Listen)
	}
	if c.Cooldown.ErrThresh != 5 {
		t.Errorf("ErrThresh = %d, want 5", c.Cooldown.ErrThresh)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 0 {
		t.Errorf("KeepaliveHours = %v, want [0] (R3)", c.Schedule.KeepaliveHours)
	}
	if c.Upstream.ResponseHeaderTimeoutSeconds != 30 {
		t.Errorf("ResponseHeaderTimeoutSeconds = %d, want 30", c.Upstream.ResponseHeaderTimeoutSeconds)
	}
	if c.ModelsFile != "./data/models.json" {
		t.Errorf("ModelsFile = %q, want ./data/models.json", c.ModelsFile)
	}
}

// TestLoadFromFile 校验 JSON 配置解析与 duration 归一化。
func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{
	  "listen": ":9999",
	  "api_key": "test-key",
	  "auth_dir": "/tmp/auths",
	  "state_file": "/tmp/state.json",
	  "models_file": "/tmp/models.json",
	  "cooldown": {"soft_rate": "5s", "err_threshold": 2, "err_cooldown": "1m"},
	  "schedule": {"keepalive_hours": [4, 16]},
	  "upstream": {"response_header_timeout_seconds": 15, "timeout_seconds": 60}
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Listen != ":9999" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.APIKey != "test-key" {
		t.Errorf("APIKey = %q", c.APIKey)
	}
	if c.SoftRateDur != 5*time.Second {
		t.Errorf("SoftRateDur = %v", c.SoftRateDur)
	}
	if c.Cooldown.ErrThresh != 2 {
		t.Errorf("ErrThresh = %d", c.Cooldown.ErrThresh)
	}
	if len(c.Schedule.KeepaliveHours) != 2 || c.Schedule.KeepaliveHours[1] != 16 {
		t.Errorf("KeepaliveHours = %v", c.Schedule.KeepaliveHours)
	}
	if c.Upstream.ResponseHeaderTimeoutSeconds != 15 || c.Upstream.TimeoutSeconds != 60 {
		t.Errorf("upstream = %d/%d", c.Upstream.ResponseHeaderTimeoutSeconds, c.Upstream.TimeoutSeconds)
	}
	if c.ModelsFile != "/tmp/models.json" {
		t.Errorf("ModelsFile = %q", c.ModelsFile)
	}
}

// TestLoadEnvOverride 校验 QC2A_* env 覆盖。
func TestLoadEnvOverride(t *testing.T) {
	os.Setenv("QC2A_API_KEY", "env-key")
	os.Setenv("QC2A_LISTEN", "127.0.0.1:7000")
	os.Setenv("QC2A_KEEPALIVE_HOURS", "2,10")
	os.Setenv("QC2A_MODELS_FILE", "/tmp/env-models.json")
	defer os.Unsetenv("QC2A_API_KEY")
	defer os.Unsetenv("QC2A_LISTEN")
	defer os.Unsetenv("QC2A_KEEPALIVE_HOURS")
	defer os.Unsetenv("QC2A_MODELS_FILE")

	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.APIKey != "env-key" {
		t.Errorf("APIKey = %q, want env-key", c.APIKey)
	}
	if c.Listen != "127.0.0.1:7000" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if len(c.Schedule.KeepaliveHours) != 2 || c.Schedule.KeepaliveHours[0] != 2 {
		t.Errorf("KeepaliveHours = %v", c.Schedule.KeepaliveHours)
	}
	if c.ModelsFile != "/tmp/env-models.json" {
		t.Errorf("ModelsFile = %q", c.ModelsFile)
	}
}

// TestLoadMissingFile 校验配置文件不存在时报错（不静默回退）。
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

// TestParseHourList 校验小时列表解析。
func TestParseHourList(t *testing.T) {
	got := parseHourList("4,16,30,abc")
	if len(got) != 2 || got[0] != 4 || got[1] != 16 {
		t.Errorf("parseHourList = %v, want [4 16]", got)
	}
}
