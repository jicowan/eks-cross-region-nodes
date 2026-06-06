package main

import (
	"os"
	"reflect"
	"testing"
)

// withArgs sets os.Args for the duration of fn and restores it afterward.
// The first two elements ("xrnctl", "<subcommand>") mirror how the real CLI is invoked,
// since the parsers read os.Args[2:].
func withArgs(args []string, fn func()) {
	orig := os.Args
	defer func() { os.Args = orig }()
	os.Args = args
	fn()
}

func TestArgValue(t *testing.T) {
	args := []string{"--cluster-name", "main"}
	if got := argValue(args, 1); got != "main" {
		t.Errorf("argValue in-range = %q, want main", got)
	}
	// Out of range (missing trailing value) returns "" instead of panicking.
	if got := argValue(args, 2); got != "" {
		t.Errorf("argValue out-of-range = %q, want empty", got)
	}
	if got := argValue(nil, 0); got != "" {
		t.Errorf("argValue on nil = %q, want empty", got)
	}
}

func TestParseAddSatelliteFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(*testing.T, *addSatelliteConfig)
	}{
		{
			name: "minimal same-account",
			args: []string{"xrnctl", "add-satellite",
				"--cluster-name", "main", "--cluster-region", "us-east-2",
				"--vpc-id", "vpc-abc", "--satellite-region", "eu-west-1"},
			check: func(t *testing.T, c *addSatelliteConfig) {
				if c.ClusterName != "main" || c.ClusterRegion != "us-east-2" {
					t.Errorf("global flags not parsed: %+v", c)
				}
				if c.VPCID != "vpc-abc" || c.SatelliteRegion != "eu-west-1" {
					t.Errorf("satellite flags not parsed: %+v", c)
				}
				if c.AccountID != "" {
					t.Errorf("AccountID should be empty when omitted, got %q", c.AccountID)
				}
			},
		},
		{
			name: "cross-account with account-id",
			args: []string{"xrnctl", "add-satellite",
				"--cluster-name", "main", "--cluster-region", "us-east-2",
				"--vpc-id", "vpc-abc", "--satellite-region", "us-west-1",
				"--account-id", "310444902345"},
			check: func(t *testing.T, c *addSatelliteConfig) {
				if c.AccountID != "310444902345" {
					t.Errorf("AccountID = %q, want 310444902345", c.AccountID)
				}
			},
		},
		{
			name: "subnet and sg lists",
			args: []string{"xrnctl", "add-satellite",
				"--cluster-name", "main", "--cluster-region", "us-east-2",
				"--vpc-id", "vpc-abc", "--satellite-region", "us-west-1",
				"--with-eniconfigs",
				"--subnet-ids", "subnet-1,subnet-2",
				"--security-group-ids", "sg-1,sg-2"},
			check: func(t *testing.T, c *addSatelliteConfig) {
				if !c.WithENIConfigs {
					t.Error("WithENIConfigs should be true")
				}
				if !reflect.DeepEqual(c.SubnetIDs, []string{"subnet-1", "subnet-2"}) {
					t.Errorf("SubnetIDs = %v", c.SubnetIDs)
				}
				if !reflect.DeepEqual(c.SecurityGroupIDs, []string{"sg-1", "sg-2"}) {
					t.Errorf("SecurityGroupIDs = %v", c.SecurityGroupIDs)
				}
			},
		},
		{
			name:    "missing vpc-id",
			args:    []string{"xrnctl", "add-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2", "--satellite-region", "us-west-1"},
			wantErr: true,
		},
		{
			name:    "missing satellite-region",
			args:    []string{"xrnctl", "add-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2", "--vpc-id", "vpc-abc"},
			wantErr: true,
		},
		{
			name:    "missing cluster flags",
			args:    []string{"xrnctl", "add-satellite", "--vpc-id", "vpc-abc", "--satellite-region", "us-west-1"},
			wantErr: true,
		},
		{
			name:    "with-eniconfigs requires security groups",
			args:    []string{"xrnctl", "add-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2", "--vpc-id", "vpc-abc", "--satellite-region", "us-west-1", "--with-eniconfigs"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"xrnctl", "add-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2", "--vpc-id", "vpc-abc", "--satellite-region", "us-west-1", "--bogus", "x"},
			wantErr: true,
		},
		{
			name:    "trailing flag without value does not panic",
			args:    []string{"xrnctl", "add-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2", "--vpc-id", "vpc-abc", "--satellite-region"},
			wantErr: true, // satellite-region ends up empty → required-field error
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withArgs(tt.args, func() {
				cfg, err := parseAddSatelliteFlags()
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
				}
				if err == nil && tt.check != nil {
					tt.check(t, cfg)
				}
			})
		})
	}
}

func TestParseRemoveSatelliteFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"valid", []string{"xrnctl", "remove-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2", "--vpc-id", "vpc-abc"}, false},
		{"missing vpc-id", []string{"xrnctl", "remove-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2"}, true},
		{"missing cluster flags", []string{"xrnctl", "remove-satellite", "--vpc-id", "vpc-abc"}, true},
		{"unknown flag", []string{"xrnctl", "remove-satellite", "--cluster-name", "main", "--cluster-region", "us-east-2", "--vpc-id", "vpc-abc", "--foo"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withArgs(tt.args, func() {
				_, err := parseRemoveSatelliteFlags()
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
				}
			})
		})
	}
}

func TestParseGlobalFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"valid", []string{"xrnctl", "list-satellites", "--cluster-name", "main", "--cluster-region", "us-east-2"}, false},
		{"missing name", []string{"xrnctl", "list-satellites", "--cluster-region", "us-east-2"}, true},
		{"missing region", []string{"xrnctl", "list-satellites", "--cluster-name", "main"}, true},
		{"unknown flag", []string{"xrnctl", "verify", "--cluster-name", "main", "--cluster-region", "us-east-2", "--nope"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withArgs(tt.args, func() {
				_, err := parseGlobalFlags()
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
				}
			})
		})
	}
}

func TestParseSetupIAMFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		check   func(*testing.T, *setupIAMConfig)
	}{
		{
			name: "same-account both halves",
			args: []string{"xrnctl", "setup-iam", "--cluster-name", "main", "--cluster-region", "us-east-2", "--node-role-name", "CrossRegionNodeRole"},
			check: func(t *testing.T, c *setupIAMConfig) {
				if c.NodeRoleName != "CrossRegionNodeRole" || c.NodeRoleOnly || c.AccessEntryOnly {
					t.Errorf("unexpected: %+v", c)
				}
			},
		},
		{
			name: "cross-account node-role-only with profile",
			args: []string{"xrnctl", "setup-iam", "--cluster-name", "main", "--cluster-region", "us-east-2", "--node-role-name", "XrnNodeRole", "--node-role-only", "--profile", "root"},
			check: func(t *testing.T, c *setupIAMConfig) {
				if !c.NodeRoleOnly || c.Profile != "root" {
					t.Errorf("unexpected: %+v", c)
				}
			},
		},
		{
			name: "cross-account access-entry-only with arn",
			args: []string{"xrnctl", "setup-iam", "--cluster-name", "main", "--cluster-region", "us-east-2", "--access-entry-only", "--node-role-arn", "arn:aws:iam::820537372947:role/XrnSatelliteNodeRole"},
			check: func(t *testing.T, c *setupIAMConfig) {
				if !c.AccessEntryOnly || c.NodeRoleARN == "" {
					t.Errorf("unexpected: %+v", c)
				}
			},
		},
		{
			name:    "both only flags is an error",
			args:    []string{"xrnctl", "setup-iam", "--cluster-name", "main", "--cluster-region", "us-east-2", "--node-role-name", "x", "--node-role-only", "--access-entry-only"},
			wantErr: true,
		},
		{
			name:    "missing node-role-name without access-entry-only",
			args:    []string{"xrnctl", "setup-iam", "--cluster-name", "main", "--cluster-region", "us-east-2"},
			wantErr: true,
		},
		{
			name:    "access-entry-only without arn or name",
			args:    []string{"xrnctl", "setup-iam", "--cluster-name", "main", "--cluster-region", "us-east-2", "--access-entry-only"},
			wantErr: true,
		},
		{
			name:    "missing cluster flags",
			args:    []string{"xrnctl", "setup-iam", "--node-role-name", "x"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"xrnctl", "setup-iam", "--cluster-name", "main", "--cluster-region", "us-east-2", "--node-role-name", "x", "--zzz"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withArgs(tt.args, func() {
				cfg, err := parseSetupIAMFlags()
				if (err != nil) != tt.wantErr {
					t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
				}
				if err == nil && tt.check != nil {
					tt.check(t, cfg)
				}
			})
		})
	}
}

func TestSplitComma(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"a,,b", []string{"a", "b"}}, // empty segments dropped
		{",a,", []string{"a"}},       // leading/trailing commas dropped
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := splitComma(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitComma(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
