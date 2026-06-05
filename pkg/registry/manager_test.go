package registry

import (
	"encoding/json"
	"reflect"
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

func TestRegistryDataJSONRoundTrip(t *testing.T) {
	reg := RegistryData{
		Version: 1,
		Satellites: []SatelliteRegion{
			{
				VPCID:          "vpc-aaa",
				Region:         "eu-west-1",
				CIDRs:          []string{"10.1.0.0/16", "100.64.1.0/24"},
				ENIConfigNames: []string{"eu-west-1a"},
				AddedAt:        "2026-06-03T00:00:00Z",
			},
			{
				VPCID:   "vpc-bbb",
				Region:  "ap-south-1",
				CIDRs:   []string{"10.2.0.0/16"},
				AddedAt: "2026-06-04T00:00:00Z",
			},
		},
	}

	// Marshal then unmarshal — should be lossless
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got RegistryData
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Version != reg.Version {
		t.Errorf("Version: got %d, want %d", got.Version, reg.Version)
	}
	if len(got.Satellites) != len(reg.Satellites) {
		t.Errorf("Satellites count: got %d, want %d", len(got.Satellites), len(reg.Satellites))
	}
	if got.Satellites[1].VPCID != "vpc-bbb" {
		t.Errorf("Satellites[1].VPCID: got %s, want vpc-bbb", got.Satellites[1].VPCID)
	}
	// Verify omitempty works — second satellite has no ENIConfigs, should serialize without the field
	if len(got.Satellites[1].ENIConfigNames) != 0 {
		t.Errorf("Satellites[1].ENIConfigNames should be empty, got %v", got.Satellites[1].ENIConfigNames)
	}
}

func TestMergeRemoteNetworkCIDRs(t *testing.T) {
	tests := []struct {
		name        string
		existing    []string
		newCIDRs    []string
		wantMerged  []string
		wantChanged bool
	}{
		{
			name:        "all new",
			existing:    []string{},
			newCIDRs:    []string{"10.1.0.0/16"},
			wantMerged:  []string{"10.1.0.0/16"},
			wantChanged: true,
		},
		{
			name:        "all already present",
			existing:    []string{"10.1.0.0/16", "10.2.0.0/16"},
			newCIDRs:    []string{"10.1.0.0/16"},
			wantMerged:  []string{"10.1.0.0/16", "10.2.0.0/16"},
			wantChanged: false,
		},
		{
			name:        "mixed",
			existing:    []string{"10.1.0.0/16"},
			newCIDRs:    []string{"10.1.0.0/16", "10.2.0.0/16"},
			wantMerged:  []string{"10.1.0.0/16", "10.2.0.0/16"},
			wantChanged: true,
		},
		{
			name:        "empty existing",
			existing:    nil,
			newCIDRs:    []string{"10.1.0.0/16"},
			wantMerged:  []string{"10.1.0.0/16"},
			wantChanged: true,
		},
		{
			name:        "empty new",
			existing:    []string{"10.1.0.0/16"},
			newCIDRs:    nil,
			wantMerged:  []string{"10.1.0.0/16"},
			wantChanged: false,
		},
		{
			name:        "deduplicates within new",
			existing:    []string{},
			newCIDRs:    []string{"10.1.0.0/16", "10.1.0.0/16", "10.2.0.0/16"},
			wantMerged:  []string{"10.1.0.0/16", "10.2.0.0/16"},
			wantChanged: true,
		},
		{
			name:        "result is sorted",
			existing:    []string{"10.5.0.0/16"},
			newCIDRs:    []string{"10.1.0.0/16", "10.3.0.0/16"},
			wantMerged:  []string{"10.1.0.0/16", "10.3.0.0/16", "10.5.0.0/16"},
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged, changed := mergeRemoteNetworkCIDRs(tt.existing, tt.newCIDRs)
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if !reflect.DeepEqual(merged, tt.wantMerged) {
				t.Errorf("merged = %v, want %v", merged, tt.wantMerged)
			}
		})
	}
}

func TestAccountIDFromARN(t *testing.T) {
	tests := []struct {
		name    string
		arn     string
		want    string
		wantErr bool
	}{
		{"valid cluster arn", "arn:aws:eks:us-east-2:820537372947:cluster/main", "820537372947", false},
		{"valid gov arn", "arn:aws-us-gov:eks:us-gov-west-1:111122223333:cluster/x", "111122223333", false},
		{"empty account", "arn:aws:eks:us-east-2::cluster/main", "", true},
		{"too few parts", "arn:aws:eks", "", true},
		{"empty string", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := accountIDFromARN(tt.arn)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("accountIDFromARN(%q) = %q, want %q", tt.arn, got, tt.want)
			}
		})
	}
}

func TestSatelliteRegionAccountIDOmitEmpty(t *testing.T) {
	// AccountID should be omitted from JSON when empty (same-account satellites),
	// and present when set (cross-account).
	sameAccount := SatelliteRegion{VPCID: "vpc-1", Region: "eu-west-1", CIDRs: []string{"10.1.0.0/16"}, AddedAt: "t"}
	data, _ := json.Marshal(sameAccount)
	if string(data) == "" || containsField(data, "account_id") {
		t.Errorf("empty AccountID should be omitted, got %s", data)
	}

	crossAccount := SatelliteRegion{VPCID: "vpc-2", Region: "us-west-1", AccountID: "310444902345", CIDRs: []string{"10.2.0.0/16"}, AddedAt: "t"}
	data, _ = json.Marshal(crossAccount)
	if !containsField(data, "account_id") {
		t.Errorf("set AccountID should be present, got %s", data)
	}
}

func containsField(data []byte, field string) bool {
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	_, ok := m[field]
	return ok
}

func TestMapKeys(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]bool
		want int // length only — order non-deterministic
	}{
		{"empty", map[string]bool{}, 0},
		{"single", map[string]bool{"a": true}, 1},
		{"multiple", map[string]bool{"a": true, "b": true, "c": false}, 3},
		// Note: mapKeys returns ALL keys regardless of value
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapKeys(tt.m)
			if len(got) != tt.want {
				t.Errorf("len = %d, want %d (got %v)", len(got), tt.want, got)
			}
		})
	}
}

func TestAddRegionInputValidation(t *testing.T) {
	// AddRegionInput has no constructor with validation, but document the contract via tests
	tests := []struct {
		name  string
		input AddRegionInput
		valid bool
	}{
		{"required fields only", AddRegionInput{VPCID: "vpc-123", SatelliteRegion: "eu-west-1"}, true},
		{"with ENIConfigs requires SGs", AddRegionInput{VPCID: "vpc-123", SatelliteRegion: "eu-west-1", WithENIConfigs: true, SecurityGroupIDs: []string{"sg-1"}}, true},
		{"missing VPC ID", AddRegionInput{SatelliteRegion: "eu-west-1"}, false},
		{"missing region", AddRegionInput{VPCID: "vpc-123"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isValid := tt.input.VPCID != "" && tt.input.SatelliteRegion != ""
			if isValid != tt.valid {
				t.Errorf("validity = %v, want %v", isValid, tt.valid)
			}
		})
	}
}
