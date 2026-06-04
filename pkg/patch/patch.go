package patch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aws/eks-cross-region-nodes/pkg/discovery"
)

var (
	kubeletEnvFile    = "/etc/eks/kubelet/environment"
	kubeletConfigFile = "/etc/kubernetes/kubelet/config.json"
	kubeconfigFile    = "/var/lib/kubelet/kubeconfig"
)

func setEnvFile(path string)        { kubeletEnvFile = path }
func setConfigFile(path string)     { kubeletConfigFile = path }
func setKubeconfigFile(path string) { kubeconfigFile = path }

func ApplyAll(ctx context.Context, cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) error {
	if err := patchKubeletEnvironment(cluster, node); err != nil {
		return fmt.Errorf("patching kubelet environment: %w", err)
	}
	if err := patchProviderID(cluster, node); err != nil {
		return fmt.Errorf("patching providerID: %w", err)
	}
	if err := patchKubeconfig(cluster, node); err != nil {
		return fmt.Errorf("patching kubeconfig region: %w", err)
	}
	return nil
}

func patchKubeletEnvironment(cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) error {
	data, err := os.ReadFile(kubeletEnvFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", kubeletEnvFile, err)
	}
	content := string(data)

	// 1. cloud-provider=external → cloud-provider= (empty)
	content = strings.Replace(content, "--cloud-provider=external", "--cloud-provider=", 1)

	// 2. hostname-override: replace whatever nodeadm set with instance ID
	if idx := strings.Index(content, "--hostname-override="); idx >= 0 {
		end := strings.IndexByte(content[idx:], ' ')
		if end == -1 {
			end = len(content) - idx
		}
		old := content[idx : idx+end]
		content = strings.Replace(content, old, "--hostname-override="+node.InstanceID, 1)
	}

	// 3. Append topology labels to existing --node-labels
	zoneLabel := "topology.kubernetes.io/zone=" + node.AvailabilityZone
	regionLabel := "topology.kubernetes.io/region=" + node.Region
	if idx := strings.Index(content, "--node-labels="); idx >= 0 {
		// Find the end of the --node-labels value (next space or EOL)
		labelStart := idx + len("--node-labels=")
		end := strings.IndexByte(content[labelStart:], ' ')
		if end == -1 {
			end = len(content) - labelStart
		}
		existingLabels := content[labelStart : labelStart+end]
		if !strings.Contains(existingLabels, "topology.kubernetes.io/zone") {
			newLabels := existingLabels + "," + zoneLabel + "," + regionLabel
			content = content[:labelStart] + newLabels + content[labelStart+end:]
		}
	}

	return os.WriteFile(kubeletEnvFile, []byte(content), 0644)
}

func patchProviderID(cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) error {
	data, err := os.ReadFile(kubeletConfigFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", kubeletConfigFile, err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parsing kubelet config JSON: %w", err)
	}

	// Set providerID to eks-hybrid format
	hybridProviderID := fmt.Sprintf("eks-hybrid:///%s/%s/%s", cluster.Region, cluster.Name, node.InstanceID)
	config["providerID"] = hybridProviderID

	out, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("marshaling kubelet config: %w", err)
	}

	return os.WriteFile(kubeletConfigFile, out, 0644)
}

func patchKubeconfig(cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) error {
	data, err := os.ReadFile(kubeconfigFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", kubeconfigFile, err)
	}
	content := string(data)

	// Replace the node's region with the cluster's region in the get-token args
	content = strings.Replace(content, "\""+node.Region+"\"", "\""+cluster.Region+"\"", 1)

	return os.WriteFile(kubeconfigFile, []byte(content), 0644)
}
