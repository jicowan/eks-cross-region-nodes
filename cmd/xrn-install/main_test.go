package main

import (
	"os"
	"testing"
)

func withArgs(args []string, fn func()) {
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = args
	fn()
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantErr   bool
		wantName  string
		wantRegion string
	}{
		{
			name:       "valid",
			args:       []string{"xrn-install", "init", "--cluster-name", "main", "--cluster-region", "us-east-2"},
			wantName:   "main",
			wantRegion: "us-east-2",
		},
		{
			name:    "missing cluster-name",
			args:    []string{"xrn-install", "init", "--cluster-region", "us-east-2"},
			wantErr: true,
		},
		{
			name:    "missing cluster-region",
			args:    []string{"xrn-install", "init", "--cluster-name", "main"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"xrn-install", "init", "--cluster-name", "main", "--cluster-region", "us-east-2", "--bogus"},
			wantErr: true,
		},
		{
			name:    "flag missing value",
			args:    []string{"xrn-install", "init", "--cluster-name"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withArgs(tt.args, func() {
				cfg, err := parseFlags()
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
				}
				if err == nil {
					if cfg.ClusterName != tt.wantName {
						t.Errorf("ClusterName = %q, want %q", cfg.ClusterName, tt.wantName)
					}
					if cfg.ClusterRegion != tt.wantRegion {
						t.Errorf("ClusterRegion = %q, want %q", cfg.ClusterRegion, tt.wantRegion)
					}
				}
			})
		})
	}
}
