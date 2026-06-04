package registry

import (
	"testing"
)

func TestCheckOverlap(t *testing.T) {
	tests := []struct {
		name     string
		newCIDRs []string
		existing []string
		wantErr  bool
	}{
		{
			name:     "no overlap",
			newCIDRs: []string{"10.1.0.0/16"},
			existing: []string{"10.0.0.0/16", "10.2.0.0/16"},
			wantErr:  false,
		},
		{
			name:     "exact match",
			newCIDRs: []string{"10.0.0.0/16"},
			existing: []string{"10.0.0.0/16"},
			wantErr:  true,
		},
		{
			name:     "new is subset of existing",
			newCIDRs: []string{"10.0.1.0/24"},
			existing: []string{"10.0.0.0/16"},
			wantErr:  true,
		},
		{
			name:     "existing is subset of new",
			newCIDRs: []string{"10.0.0.0/8"},
			existing: []string{"10.1.0.0/16"},
			wantErr:  true,
		},
		{
			name:     "completely disjoint",
			newCIDRs: []string{"172.16.0.0/16"},
			existing: []string{"10.0.0.0/16", "10.1.0.0/16"},
			wantErr:  false,
		},
		{
			name:     "multiple new CIDRs, one overlaps",
			newCIDRs: []string{"10.3.0.0/16", "10.0.0.0/16"},
			existing: []string{"10.0.0.0/16"},
			wantErr:  true,
		},
		{
			name:     "empty existing",
			newCIDRs: []string{"10.1.0.0/16"},
			existing: nil,
			wantErr:  false,
		},
		{
			name:     "empty new",
			newCIDRs: nil,
			existing: []string{"10.0.0.0/16"},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkOverlap(tt.newCIDRs, tt.existing)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkOverlap() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDedupe(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  int
	}{
		{"no duplicates", []string{"a", "b", "c"}, 3},
		{"all duplicates", []string{"a", "a", "a"}, 1},
		{"some duplicates", []string{"10.0.0.0/16", "10.1.0.0/16", "10.0.0.0/16"}, 2},
		{"empty", nil, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupe(tt.input)
			if len(got) != tt.want {
				t.Errorf("dedupe() returned %d items, want %d", len(got), tt.want)
			}
		})
	}
}

func TestRegistryDataSerialization(t *testing.T) {
	reg := RegistryData{
		Version: 1,
		Satellites: []SatelliteRegion{
			{
				VPCID:          "vpc-123",
				Region:         "eu-west-1",
				CIDRs:          []string{"10.1.0.0/16"},
				ENIConfigNames: []string{"eu-west-1a", "eu-west-1b"},
				AddedAt:        "2026-06-03T00:00:00Z",
			},
		},
	}

	if len(reg.Satellites) != 1 {
		t.Fatalf("expected 1 satellite, got %d", len(reg.Satellites))
	}
	if reg.Satellites[0].Region != "eu-west-1" {
		t.Errorf("region = %s, want eu-west-1", reg.Satellites[0].Region)
	}
	if len(reg.Satellites[0].CIDRs) != 1 || reg.Satellites[0].CIDRs[0] != "10.1.0.0/16" {
		t.Errorf("CIDRs = %v, want [10.1.0.0/16]", reg.Satellites[0].CIDRs)
	}
}
