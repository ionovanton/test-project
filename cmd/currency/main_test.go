package main

import (
	"testing"
	"time"
)

func TestLoadConfig_HTTPTimeout(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", value: "", want: defaultHTTPTimeout},
		{name: "custom", value: "3s", want: 3 * time.Second},
		{name: "invalid", value: "abc", wantErr: true},
		{name: "zero", value: "0s", wantErr: true},
		{name: "negative", value: "-1s", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://unused")
			t.Setenv("BANK2_TOKEN", "token")
			t.Setenv("HTTP_TIMEOUT", tt.value)

			cfg, err := loadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("loadConfig() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig() unexpected error = %v", err)
			}
			if cfg.httpTimeout != tt.want {
				t.Fatalf("httpTimeout = %s, want %s", cfg.httpTimeout, tt.want)
			}
		})
	}
}
