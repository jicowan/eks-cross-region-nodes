package patch

import (
	"context"
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

func TestRenderCredentialHelper(t *testing.T) {
	s := RenderCredentialHelper("main", "us-east-2", "arn:aws:iam::820537372947:role/XrnSatelliteNodeRole", "")

	for _, want := range []string{
		"#!/bin/bash",
		"aws sts assume-role --region us-east-2 --role-arn arn:aws:iam::820537372947:role/XrnSatelliteNodeRole",
		`--role-session-name "$INSTANCE_ID"`, // identity must be system:node:<instance-id>
		"exec aws eks get-token --cluster-name main --region us-east-2",
		"169.254.169.254/latest/meta-data/instance-id",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("helper script missing %q\n---\n%s", want, s)
		}
	}
	// No external-id arg when none given.
	if strings.Contains(s, "--external-id") {
		t.Error("helper should not include --external-id when empty")
	}
}

func TestRenderCredentialHelper_ExternalID(t *testing.T) {
	s := RenderCredentialHelper("main", "us-east-2", "arn:aws:iam::111:role/Sat", "my-ext-id")
	if !strings.Contains(s, "--external-id my-ext-id") {
		t.Errorf("helper should include external-id arg\n%s", s)
	}
}

func TestInstallCredentialHelper(t *testing.T) {
	tmpDir := t.TempDir()
	kcFile := filepath.Join(tmpDir, "kubeconfig")
	helperFile := filepath.Join(tmpDir, "xrn", "get-cluster-token.sh")

	// A realistic nodeadm-generated kubeconfig with a get-token exec block.
	os.WriteFile(kcFile, []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://example.gr7.us-east-2.eks.amazonaws.com
  name: main
users:
- name: kubelet
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args:
      - eks
      - get-token
      - --cluster-name
      - main
      - --region
      - us-west-1
`), 0644)

	origKc, origHelper := kubeconfigFile, credHelperPath
	defer func() { setKubeconfigFile(origKc); setCredHelperPath(origHelper) }()
	setKubeconfigFile(kcFile)
	setCredHelperPath(helperFile)

	cluster := &discovery.ClusterInfo{Name: "main", Region: "us-east-2"}
	node := &discovery.NodeMetadata{InstanceID: "i-0abc", Region: "us-west-1"}
	xacct := &CrossAccount{Enabled: true, SatelliteRoleARN: "arn:aws:iam::820537372947:role/XrnSatelliteNodeRole"}

	if err := installCredentialHelper(cluster, node, xacct); err != nil {
		t.Fatalf("installCredentialHelper: %v", err)
	}

	// Helper script exists and is executable.
	info, err := os.Stat(helperFile)
	if err != nil {
		t.Fatalf("helper not written: %v", err)
	}
	if info.Mode().Perm()&0100 == 0 {
		t.Errorf("helper not executable, mode = %v", info.Mode())
	}

	// kubeconfig exec now points at the helper.
	data, _ := os.ReadFile(kcFile)
	if !strings.Contains(string(data), helperFile) {
		t.Errorf("kubeconfig exec not rewritten to helper path:\n%s", data)
	}
	// The old direct get-token args should be gone.
	if strings.Contains(string(data), "get-token") {
		t.Errorf("old get-token exec args should be replaced:\n%s", data)
	}
}

func TestInstallCredentialHelper_RequiresRoleARN(t *testing.T) {
	cluster := &discovery.ClusterInfo{Name: "main", Region: "us-east-2"}
	node := &discovery.NodeMetadata{InstanceID: "i-0abc"}
	err := installCredentialHelper(cluster, node, &CrossAccount{Enabled: true})
	if err == nil {
		t.Fatal("expected error when SatelliteRoleARN is empty")
	}
}

func TestApplyAll(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up all three target files with realistic nodeadm-generated content
	envFile := filepath.Join(tmpDir, "environment")
	configFile := filepath.Join(tmpDir, "config.json")
	kcFile := filepath.Join(tmpDir, "kubeconfig")

	os.WriteFile(envFile, []byte(`NODEADM_KUBELET_ARGS=--cloud-provider=external --hostname-override=ip-10-1-2-112.eu-west-1.compute.internal --node-labels=eks.amazonaws.com/compute-type=cross-region`), 0644)

	originalConfig := map[string]interface{}{
		"kind":       "KubeletConfiguration",
		"providerID": "aws:///eu-west-1b/i-0a5ecec7f33053b35",
	}
	data, _ := json.MarshalIndent(originalConfig, "", "    ")
	os.WriteFile(configFile, data, 0644)

	os.WriteFile(kcFile, []byte(`apiVersion: v1
users:
  - name: kubelet
    user:
      exec:
        args:
          - "--region"
          - "eu-west-1"
`), 0644)

	// Override file paths
	origEnv, origCfg, origKc := kubeletEnvFile, kubeletConfigFile, kubeconfigFile
	defer func() {
		setEnvFile(origEnv)
		setConfigFile(origCfg)
		setKubeconfigFile(origKc)
	}()
	setEnvFile(envFile)
	setConfigFile(configFile)
	setKubeconfigFile(kcFile)

	cluster := &discovery.ClusterInfo{Name: "main", Region: "us-east-2"}
	node := &discovery.NodeMetadata{
		InstanceID:       "i-0a5ecec7f33053b35",
		Region:           "eu-west-1",
		AvailabilityZone: "eu-west-1b",
	}

	if err := ApplyAll(context.Background(), cluster, node, nil); err != nil {
		t.Fatalf("ApplyAll failed: %v", err)
	}

	// Verify env file: cloud-provider, hostname, labels all updated
	envContent, _ := os.ReadFile(envFile)
	envStr := string(envContent)
	if strings.Contains(envStr, "--cloud-provider=external") {
		t.Errorf("ApplyAll: cloud-provider not changed: %s", envStr)
	}
	if !strings.Contains(envStr, "--hostname-override=i-0a5ecec7f33053b35") {
		t.Errorf("ApplyAll: hostname not changed: %s", envStr)
	}
	if !strings.Contains(envStr, "topology.kubernetes.io/zone=eu-west-1b") {
		t.Errorf("ApplyAll: zone label not added: %s", envStr)
	}

	// Verify config.json: providerID updated
	configContent, _ := os.ReadFile(configFile)
	var resultConfig map[string]interface{}
	json.Unmarshal(configContent, &resultConfig)
	wantPID := "eks-hybrid:///us-east-2/main/i-0a5ecec7f33053b35"
	if resultConfig["providerID"] != wantPID {
		t.Errorf("ApplyAll: providerID = %v, want %s", resultConfig["providerID"], wantPID)
	}

	// Verify kubeconfig: region updated
	kcContent, _ := os.ReadFile(kcFile)
	if strings.Contains(string(kcContent), `"eu-west-1"`) {
		t.Errorf("ApplyAll: kubeconfig still has node region: %s", string(kcContent))
	}
	if !strings.Contains(string(kcContent), `"us-east-2"`) {
		t.Errorf("ApplyAll: kubeconfig missing cluster region: %s", string(kcContent))
	}
}
