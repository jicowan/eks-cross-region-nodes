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

func TestResultsAllPassedAllPassing(t *testing.T) {
	results := &Results{
		checks: []CheckResult{
			{Name: "check1", Passed: true},
			{Name: "check2", Passed: true},
		},
	}
	if !results.AllPassed() {
		t.Error("AllPassed() = false, want true")
	}
	if len(results.Failed()) != 0 {
		t.Errorf("Failed() should be empty, got %d", len(results.Failed()))
	}
	if len(results.Warnings()) != 0 {
		t.Errorf("Warnings() should be empty, got %d", len(results.Warnings()))
	}
	if results.FirstFailedExitCode() != 0 {
		t.Errorf("FirstFailedExitCode() = %d, want 0", results.FirstFailedExitCode())
	}
}

func TestResultsWarningsDoNotBlock(t *testing.T) {
	// Warnings (Passed=false + Warning=true) should NOT cause AllPassed=false
	// or appear in Failed(), but should appear in Warnings().
	results := &Results{
		checks: []CheckResult{
			{Name: "blocking-pass", Passed: true},
			{Name: "warning", Passed: false, Warning: true, Error: "advisory issue", ExitCode: 18},
			{Name: "another-pass", Passed: true},
		},
	}

	if !results.AllPassed() {
		t.Error("AllPassed() = false; warnings should not block install")
	}
	if len(results.Failed()) != 0 {
		t.Errorf("Failed() returned %d results, want 0 (warnings should not be in Failed)", len(results.Failed()))
	}
	warnings := results.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("Warnings() returned %d results, want 1", len(warnings))
	}
	if warnings[0].Name != "warning" {
		t.Errorf("Warnings()[0].Name = %s, want warning", warnings[0].Name)
	}
	if results.FirstFailedExitCode() != 0 {
		t.Errorf("FirstFailedExitCode() = %d, want 0 (warnings don't fail)", results.FirstFailedExitCode())
	}
}

func TestResultsAll(t *testing.T) {
	// All() returns checks in insertion order
	results := &Results{
		checks: []CheckResult{
			{Name: "first", Passed: true},
			{Name: "second", Passed: false},
			{Name: "third", Passed: false, Warning: true},
		},
	}
	all := results.All()
	if len(all) != 3 {
		t.Fatalf("All() length = %d, want 3", len(all))
	}
	if all[0].Name != "first" || all[1].Name != "second" || all[2].Name != "third" {
		t.Errorf("All() order wrong: %v", all)
	}
}

func TestParseCIDR(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"10.0.0.0/16", false},
		{"172.16.0.0/12", false},
		{"not-a-cidr", true},
		{"10.0.0.0", true}, // missing prefix
		{"", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ipnet, err := parseCIDR(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseCIDR(%q) err=%v, wantErr=%v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && ipnet == nil {
				t.Errorf("parseCIDR(%q): got nil ipnet, want non-nil", tt.input)
			}
		})
	}
}

func TestCheckEndpointDNSResolvable(t *testing.T) {
	// Use a hostname that always resolves
	cluster := &discovery.ClusterInfo{Endpoint: "https://aws.amazon.com"}
	r := checkEndpointDNS(cluster)
	if !r.Passed {
		t.Errorf("checkEndpointDNS for aws.amazon.com should pass, got error: %s", r.Error)
	}
}

func TestCheckEndpointDNSUnresolvable(t *testing.T) {
	// Use a hostname that should never resolve
	cluster := &discovery.ClusterInfo{Endpoint: "https://this-host-does-not-exist-anywhere-12345.example.invalid"}
	r := checkEndpointDNS(cluster)
	if r.Passed {
		t.Errorf("checkEndpointDNS for invalid hostname should fail, but passed")
	}
}

func TestResultsBlockingFailureBeatsWarning(t *testing.T) {
	// Mix of warnings AND a real failure: AllPassed should be false, FirstFailedExitCode
	// should return the real failure's exit code (not the warning's).
	results := &Results{
		checks: []CheckResult{
			{Name: "warning", Passed: false, Warning: true, ExitCode: 18},
			{Name: "blocking-fail", Passed: false, ExitCode: 13, Error: "real failure"},
		},
	}

	if results.AllPassed() {
		t.Error("AllPassed() = true; should be false because of blocking failure")
	}
	if got := results.FirstFailedExitCode(); got != 13 {
		t.Errorf("FirstFailedExitCode() = %d, want 13 (blocking failure, not 18 from warning)", got)
	}
	if len(results.Failed()) != 1 {
		t.Errorf("Failed() returned %d, want 1", len(results.Failed()))
	}
	if len(results.Warnings()) != 1 {
		t.Errorf("Warnings() returned %d, want 1", len(results.Warnings()))
	}
}
