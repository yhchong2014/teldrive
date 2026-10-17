package config

import (
	"strings"
	"testing"
)

func TestWebDAVDefaults(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if !cfg.WebDAV.Enabled || cfg.WebDAV.Prefix != "/webdav" || cfg.WebDAV.ReadOnly || cfg.WebDAV.EncryptUploads || cfg.WebDAV.UploadPartSize != 512<<20 {
		t.Fatalf("WebDAV defaults = %#v", cfg.WebDAV)
	}
	if err := validTestConfig().Validate(); err != nil {
		t.Fatalf("default WebDAV configuration rejected: %v", err)
	}
}

func TestValidateWebDAV(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{name: "relative prefix", mutate: func(cfg *Config) { cfg.WebDAV.Prefix = "webdav" }, message: "absolute path"},
		{name: "root prefix", mutate: func(cfg *Config) { cfg.WebDAV.Prefix = "/" }, message: "absolute path"},
		{name: "trailing slash", mutate: func(cfg *Config) { cfg.WebDAV.Prefix = "/dav/" }, message: "absolute path"},
		{name: "api collision", mutate: func(cfg *Config) { cfg.WebDAV.Prefix = "/api/dav" }, message: "collides with /api"},
		{name: "v1 collision", mutate: func(cfg *Config) { cfg.WebDAV.Prefix = "/v1" }, message: "collides with /v1"},
		{name: "small parts", mutate: func(cfg *Config) { cfg.WebDAV.UploadPartSize = 1 << 20 }, message: "upload-part-size"},
		{name: "large parts", mutate: func(cfg *Config) { cfg.WebDAV.UploadPartSize = 4 << 30 }, message: "upload-part-size"},
		{name: "encryption without key", mutate: func(cfg *Config) { cfg.WebDAV.EncryptUploads = true }, message: "active encryption key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := validTestConfig()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Validate() error = %v, want %q", err, test.message)
			}
		})
	}
	disabled := validTestConfig()
	disabled.WebDAV.Enabled = false
	disabled.WebDAV.Prefix = "/api"
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled WebDAV prefix was validated: %v", err)
	}
	custom := validTestConfig()
	custom.WebDAV.Prefix = "/dav/drive"
	if err := custom.Validate(); err != nil {
		t.Fatalf("nested prefix rejected: %v", err)
	}
}
