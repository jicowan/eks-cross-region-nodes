package preflight

import (
	"net"
	"testing"

	"github.com/aws/eks-cross-region-nodes/pkg/discovery"
)

func TestCIDRsOverlap(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		overlap bool
	}{
		{"same CIDR", "10.0.0.0/16", "10.0.0.0/16", true},
		{"a contains b", "10.0.0.0/8", "10.1.0.0/16", true},
		{"b contains a", "10.1.0.0/16", "10.0.0.0/8", true},
		{"no overlap", "10.0.0.0/16", "10.1.0.0/16", false},
		{"adjacent no overlap", "10.0.0.0/24", "10.0.1.0/24", false},
		{"partial overlap", "10.0.0.0/15", "10.1.0.0/16", true},
		{"completely disjoint", "172.16.0.0/16", "10.0.0.0/8", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, aNet, _ := net.ParseCIDR(tt.a)
			_, bNet, _ := net.ParseCIDR(tt.b)
			got := cidrsOverlap(aNet, bNet)
			if got != tt.overlap {
				t.Errorf("cidrsOverlap(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.overlap)
			}
		})
	}
}

func TestCheckIMDS(t *testing.T) {
	tests := []struct {
		name   string
		node   *discovery.NodeMetadata
		passed bool
	}{
		{"all present", &discovery.NodeMetadata{InstanceID: "i-123", Region: "us-east-1", AvailabilityZone: "us-east-1a"}, true},
		{"missing instance ID", &discovery.NodeMetadata{InstanceID: "", Region: "us-east-1", AvailabilityZone: "us-east-1a"}, false},
		{"missing region", &discovery.NodeMetadata{InstanceID: "i-123", Region: "", AvailabilityZone: "us-east-1a"}, false},
		{"missing AZ", &discovery.NodeMetadata{InstanceID: "i-123", Region: "us-east-1", AvailabilityZone: ""}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := checkIMDS(tt.node)
			if r.Passed != tt.passed {
				t.Errorf("checkIMDS passed=%v, want %v (error: %s)", r.Passed, tt.passed, r.Error)
			}
		})
	}
}

func TestCheckCrossRegion(t *testing.T) {
	tests := []struct {
		name          string
		clusterRegion string
		nodeRegion    string
		passed        bool
	}{
		{"different regions", "us-east-2", "eu-west-1", true},
		{"same region", "us-east-2", "us-east-2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &discovery.ClusterInfo{Region: tt.clusterRegion}
			node := &discovery.NodeMetadata{Region: tt.nodeRegion}
			r := checkCrossRegion(cluster, node)
			if r.Passed != tt.passed {
				t.Errorf("checkCrossRegion passed=%v, want %v (error: %s)", r.Passed, tt.passed, r.Error)
			}
		})
	}
}

func TestResultsMethods(t *testing.T) {
	results := &Results{
		checks: []CheckResult{
			{Name: "check1", Passed: true, ExitCode: 10},
			{Name: "check2", Passed: false, ExitCode: 13, Error: "something broke"},
			{Name: "check3", Passed: true, ExitCode: 14},
		},
	}

	if results.AllPassed() {
		t.Error("AllPassed() = true, want false")
	}

	failed := results.Failed()
	if len(failed) != 1 {
		t.Errorf("Failed() returned %d results, want 1", len(failed))
	}
	if failed[0].Name != "check2" {
		t.Errorf("Failed()[0].Name = %s, want check2", failed[0].Name)
	}

	if code := results.FirstFailedExitCode(); code != 13 {
		t.Errorf("FirstFailedExitCode() = %d, want 13", code)
	}
}
