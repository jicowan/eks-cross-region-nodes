package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/aws/eks-cross-region-nodes/pkg/discovery"
)

const nodeConfigTemplate = `---
apiVersion: node.eks.aws/v1alpha1
kind: NodeConfig
spec:
  cluster:
    name: {{.Cluster.Name}}
    region: {{.Cluster.Region}}
    apiServerEndpoint: {{.Cluster.Endpoint}}
    certificateAuthority: {{.Cluster.CertificateAuth}}
    cidr: {{.Cluster.ServiceCIDR}}
  kubelet:
    flags:
      - --node-labels=eks.amazonaws.com/compute-type=cross-region
`

const nodeConfigPath = "/etc/eks/xrn-nodeconfig.yaml"

// NodeadmAlreadyRan returns true if nodeadm has already bootstrapped this node.
// It checks for the kubeconfig file that nodeadm writes on successful init.
func NodeadmAlreadyRan() bool {
	if _, err := os.Stat("/var/lib/kubelet/kubeconfig"); err == nil {
		return true
	}
	return false
}

// RunNodeadmIfNeeded runs nodeadm init only if it hasn't already bootstrapped.
// If nodeadm already ran (detected by the presence of /var/lib/kubelet/kubeconfig),
// it skips the bootstrap and returns nil.
func RunNodeadmIfNeeded(ctx context.Context, cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) (skipped bool, err error) {
	if NodeadmAlreadyRan() {
		return true, nil
	}

	tmpl, err := template.New("nodeconfig").Parse(nodeConfigTemplate)
	if err != nil {
		return false, fmt.Errorf("parsing template: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(nodeConfigPath), 0755); err != nil {
		return false, fmt.Errorf("creating config directory: %w", err)
	}

	f, err := os.Create(nodeConfigPath)
	if err != nil {
		return false, fmt.Errorf("creating nodeconfig file: %w", err)
	}
	defer f.Close()

	data := struct {
		Cluster *discovery.ClusterInfo
		Node    *discovery.NodeMetadata
	}{cluster, node}

	if err := tmpl.Execute(f, data); err != nil {
		return false, fmt.Errorf("writing nodeconfig: %w", err)
	}

	cmd := exec.CommandContext(ctx, "nodeadm", "init", "--config-source", "file://"+nodeConfigPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("nodeadm init failed: %w", err)
	}

	return false, nil
}

func RestartKubelet(ctx context.Context) error {
	// daemon-reload first to pick up any systemd changes
	if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := exec.CommandContext(ctx, "systemctl", "restart", "kubelet").Run(); err != nil {
		return fmt.Errorf("systemctl restart kubelet: %w", err)
	}
	return nil
}
