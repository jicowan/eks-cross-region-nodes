package patch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/eks-cross-region-nodes/pkg/discovery"
)

func TestPatchKubeletEnvironment(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, "environment")

	original := `NODEADM_KUBELET_ARGS=--runtime-cgroups=/runtime.slice/containerd.service --config=/etc/kubernetes/kubelet/config.json --kubeconfig=/var/lib/kubelet/kubeconfig --node-ip=10.1.2.112 --cloud-provider=external --hostname-override=ip-10-1-2-112.eu-west-1.compute.internal --node-labels=eks.amazonaws.com/compute-type=cross-region`
	os.WriteFile(envFile, []byte(original), 0644)

	// Override the file path for testing
	origEnvFile := kubeletEnvFile
	defer func() { setEnvFile(origEnvFile) }()
	setEnvFile(envFile)

	cluster := &discovery.ClusterInfo{
		Name:   "main",
		Region: "us-east-2",
	}
	node := &discovery.NodeMetadata{
		InstanceID:       "i-0a5ecec7f33053b35",
		Region:           "eu-west-1",
		AvailabilityZone: "eu-west-1b",
	}

	err := patchKubeletEnvironment(cluster, node)
	if err != nil {
		t.Fatalf("patchKubeletEnvironment failed: %v", err)
	}

	data, _ := os.ReadFile(envFile)
	result := string(data)

	// cloud-provider should be empty
	if !strings.Contains(result, "--cloud-provider=") || strings.Contains(result, "--cloud-provider=external") {
		t.Errorf("cloud-provider not patched correctly. Got: %s", result)
	}

	// hostname-override should be instance ID
	if !strings.Contains(result, "--hostname-override=i-0a5ecec7f33053b35") {
		t.Errorf("hostname-override not patched correctly. Got: %s", result)
	}

	// topology labels should be appended
	if !strings.Contains(result, "topology.kubernetes.io/zone=eu-west-1b") {
		t.Errorf("topology zone label not added. Got: %s", result)
	}
	if !strings.Contains(result, "topology.kubernetes.io/region=eu-west-1") {
		t.Errorf("topology region label not added. Got: %s", result)
	}

	// original label should still be there
	if !strings.Contains(result, "eks.amazonaws.com/compute-type=cross-region") {
		t.Errorf("original label lost. Got: %s", result)
	}
}

func TestPatchKubeletEnvironmentIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, "environment")

	// Already patched content
	alreadyPatched := `NODEADM_KUBELET_ARGS=--cloud-provider= --hostname-override=i-0a5ecec7f33053b35 --node-labels=eks.amazonaws.com/compute-type=cross-region,topology.kubernetes.io/zone=eu-west-1b,topology.kubernetes.io/region=eu-west-1`
	os.WriteFile(envFile, []byte(alreadyPatched), 0644)

	origEnvFile := kubeletEnvFile
	defer func() { setEnvFile(origEnvFile) }()
	setEnvFile(envFile)

	cluster := &discovery.ClusterInfo{Region: "us-east-2", Name: "main"}
	node := &discovery.NodeMetadata{
		InstanceID:       "i-0a5ecec7f33053b35",
		Region:           "eu-west-1",
		AvailabilityZone: "eu-west-1b",
	}

	err := patchKubeletEnvironment(cluster, node)
	if err != nil {
		t.Fatalf("patchKubeletEnvironment failed on idempotent run: %v", err)
	}

	data, _ := os.ReadFile(envFile)
	result := string(data)

	// Should not double-append topology labels
	if strings.Count(result, "topology.kubernetes.io/zone") > 1 {
		t.Errorf("topology zone label duplicated. Got: %s", result)
	}
}

func TestPatchProviderID(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.json")

	originalConfig := map[string]interface{}{
		"kind":       "KubeletConfiguration",
		"providerID": "aws:///eu-west-1b/i-0a5ecec7f33053b35",
		"clusterDNS": []string{"172.20.0.10"},
	}
	data, _ := json.MarshalIndent(originalConfig, "", "    ")
	os.WriteFile(configFile, data, 0644)

	origConfigFile := kubeletConfigFile
	defer func() { setConfigFile(origConfigFile) }()
	setConfigFile(configFile)

	cluster := &discovery.ClusterInfo{Region: "us-east-2", Name: "main"}
	node := &discovery.NodeMetadata{InstanceID: "i-0a5ecec7f33053b35"}

	err := patchProviderID(cluster, node)
	if err != nil {
		t.Fatalf("patchProviderID failed: %v", err)
	}

	data, _ = os.ReadFile(configFile)
	var result map[string]interface{}
	json.Unmarshal(data, &result)

	expected := "eks-hybrid:///us-east-2/main/i-0a5ecec7f33053b35"
	if result["providerID"] != expected {
		t.Errorf("providerID = %v, want %s", result["providerID"], expected)
	}
}

func TestPatchKubeconfig(t *testing.T) {
	tmpDir := t.TempDir()
	kcFile := filepath.Join(tmpDir, "kubeconfig")

	original := `apiVersion: v1
kind: Config
users:
  - name: kubelet
    user:
      exec:
        command: aws
        args:
          - "eks"
          - "get-token"
          - "--cluster-name"
          - "main"
          - "--region"
          - "eu-west-1"
`
	os.WriteFile(kcFile, []byte(original), 0644)

	origKcFile := kubeconfigFile
	defer func() { setKubeconfigFile(origKcFile) }()
	setKubeconfigFile(kcFile)

	cluster := &discovery.ClusterInfo{Region: "us-east-2"}
	node := &discovery.NodeMetadata{Region: "eu-west-1"}

	err := patchKubeconfig(cluster, node)
	if err != nil {
		t.Fatalf("patchKubeconfig failed: %v", err)
	}

	data, _ := os.ReadFile(kcFile)
	result := string(data)

	if strings.Contains(result, "eu-west-1") {
		t.Errorf("node region not replaced. Got: %s", result)
	}
	if !strings.Contains(result, "us-east-2") {
		t.Errorf("cluster region not set. Got: %s", result)
	}
}
