package main

import (
	"os"
	"strings"
	"testing"

	"github.com/aws/eks-cross-region-nodes/pkg/discovery"
)

func withArgs(args []string, fn func()) {
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = args
	fn()
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		wantName   string
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
		{
			name:       "cross-account flags",
			args:       []string{"xrn-install", "init", "--cluster-name", "main", "--cluster-region", "us-east-2", "--cluster-account-role-arn", "arn:aws:iam::820537372947:role/XrnSatelliteNodeRole", "--cluster-account-external-id", "ext1"},
			wantName:   "main",
			wantRegion: "us-east-2",
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

func TestResolveCrossAccount(t *testing.T) {
	clusterIn := func(acct string) *discovery.ClusterInfo {
		return &discovery.ClusterInfo{Name: "main", Region: "us-east-2", AccountID: acct}
	}
	nodeIn := func(acct string) *discovery.NodeMetadata {
		return &discovery.NodeMetadata{InstanceID: "i-0abc", AccountID: acct}
	}

	t.Run("same account → nil (no flag needed)", func(t *testing.T) {
		cfg := &config{ClusterName: "main", ClusterRegion: "us-east-2"}
		xa, err := resolveCrossAccount(cfg, clusterIn("820537372947"), nodeIn("820537372947"))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if xa != nil {
			t.Errorf("expected nil cross-account for same account, got %+v", xa)
		}
	})

	t.Run("mismatch + flag → cross-account config", func(t *testing.T) {
		cfg := &config{
			ClusterName: "main", ClusterRegion: "us-east-2",
			ClusterAccountRoleARN: "arn:aws:iam::820537372947:role/XrnSatelliteNodeRole",
			ClusterAccountExtID:   "ext1",
		}
		xa, err := resolveCrossAccount(cfg, clusterIn("820537372947"), nodeIn("310444902345"))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if xa == nil || !xa.Enabled {
			t.Fatal("expected enabled cross-account config")
		}
		if xa.SatelliteRoleARN != cfg.ClusterAccountRoleARN || xa.ExternalID != "ext1" {
			t.Errorf("cross-account config not populated: %+v", xa)
		}
	})

	t.Run("mismatch + no flag → error with hint", func(t *testing.T) {
		cfg := &config{ClusterName: "main", ClusterRegion: "us-east-2"}
		_, err := resolveCrossAccount(cfg, clusterIn("820537372947"), nodeIn("310444902345"))
		if err == nil {
			t.Fatal("expected error when accounts differ but flag is absent")
		}
		if !strings.Contains(err.Error(), "--cluster-account-role-arn") {
			t.Errorf("error should hint at the flag: %v", err)
		}
	})

	t.Run("unknown cluster account + flag → uses flag", func(t *testing.T) {
		cfg := &config{ClusterName: "main", ClusterRegion: "us-east-2", ClusterAccountRoleARN: "arn:aws:iam::820537372947:role/X"}
		xa, err := resolveCrossAccount(cfg, clusterIn(""), nodeIn("310444902345"))
		if err != nil || xa == nil {
			t.Fatalf("expected flag to drive cross-account when cluster acct unknown; xa=%v err=%v", xa, err)
		}
	})
}
