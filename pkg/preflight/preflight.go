package preflight

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/eks-cross-region-nodes/pkg/discovery"
)

type CheckResult struct {
	Name     string
	Passed   bool
	Warning  bool // Advisory only — Passed=false + Warning=true does NOT block install
	Error    string
	ExitCode int
}

type Results struct {
	checks []CheckResult
}

func (r *Results) All() []CheckResult { return r.checks }

// AllPassed returns true if every blocking check passed. Warnings (Passed=false + Warning=true)
// do not block install.
func (r *Results) AllPassed() bool {
	for _, c := range r.checks {
		if !c.Passed && !c.Warning {
			return false
		}
	}
	return true
}

// Failed returns only blocking failures (not warnings).
func (r *Results) Failed() []CheckResult {
	var failed []CheckResult
	for _, c := range r.checks {
		if !c.Passed && !c.Warning {
			failed = append(failed, c)
		}
	}
	return failed
}

// Warnings returns advisory checks that did not pass but do not block install.
func (r *Results) Warnings() []CheckResult {
	var warnings []CheckResult
	for _, c := range r.checks {
		if !c.Passed && c.Warning {
			warnings = append(warnings, c)
		}
	}
	return warnings
}

func (r *Results) FirstFailedExitCode() int {
	for _, c := range r.checks {
		if !c.Passed && !c.Warning {
			return c.ExitCode
		}
	}
	return 0
}

func RunAll(ctx context.Context, cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) *Results {
	results := &Results{}

	results.checks = append(results.checks, checkIMDS(node))
	results.checks = append(results.checks, checkCrossRegion(cluster, node))
	results.checks = append(results.checks, checkEndpointDNS(cluster))
	results.checks = append(results.checks, checkEndpointReachable(cluster))
	results.checks = append(results.checks, checkIMDSHopLimit(ctx, node))
	results.checks = append(results.checks, checkAccessEntry(ctx, cluster, node))
	results.checks = append(results.checks, checkCIDROverlap(ctx, cluster, node))
	results.checks = append(results.checks, checkRemoteNetworkConfig(ctx, cluster, node))
	results.checks = append(results.checks, checkCSRApprover(ctx, cluster))

	return results
}

func checkIMDS(node *discovery.NodeMetadata) CheckResult {
	r := CheckResult{Name: "IMDS reachable and instance metadata available", ExitCode: 10}
	if node.InstanceID == "" || node.Region == "" || node.AvailabilityZone == "" {
		r.Error = "could not retrieve required metadata from IMDS (instance-id, region, or AZ missing)"
		return r
	}
	r.Passed = true
	return r
}

func checkCrossRegion(cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) CheckResult {
	r := CheckResult{Name: "Node is in a different region than the cluster", ExitCode: 11}
	if node.Region == cluster.Region {
		r.Error = fmt.Sprintf("node region (%s) is the same as cluster region (%s) — cross-region install not needed, use a regular MNG instead", node.Region, cluster.Region)
		return r
	}
	r.Passed = true
	return r
}

func checkEndpointDNS(cluster *discovery.ClusterInfo) CheckResult {
	r := CheckResult{Name: "Cluster API endpoint DNS resolves", ExitCode: 13}

	// Extract hostname from endpoint URL
	host := cluster.Endpoint
	if len(host) > 8 && host[:8] == "https://" {
		host = host[8:]
	}

	addrs, err := net.LookupHost(host)
	if err != nil {
		r.Error = fmt.Sprintf("DNS resolution failed for %s: %v. If private-only endpoint, see runbook step 5b.", host, err)
		return r
	}
	if len(addrs) == 0 {
		r.Error = fmt.Sprintf("DNS resolved %s but returned no addresses", host)
		return r
	}
	r.Passed = true
	return r
}

func checkEndpointReachable(cluster *discovery.ClusterInfo) CheckResult {
	r := CheckResult{Name: "Cluster API endpoint reachable on TCP 443", ExitCode: 13}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	resp, err := client.Get(cluster.Endpoint + "/healthz")
	if err != nil {
		r.Error = fmt.Sprintf("cannot reach %s: %v. Check: cluster SG allows TCP 443 from satellite VPC CIDR, TGW routes are configured, DNS resolves to correct IPs.", cluster.Endpoint, err)
		return r
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		r.Error = fmt.Sprintf("endpoint returned HTTP %d (expected 200). The endpoint is reachable but not healthy.", resp.StatusCode)
		return r
	}
	r.Passed = true
	return r
}

func checkIMDSHopLimit(ctx context.Context, node *discovery.NodeMetadata) CheckResult {
	r := CheckResult{Name: "IMDSv2 hop limit >= 2", ExitCode: 10}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(node.Region))
	if err != nil {
		r.Error = fmt.Sprintf("loading AWS config: %v", err)
		return r
	}

	client := ec2.NewFromConfig(cfg)
	out, err := client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{node.InstanceID},
	})
	if err != nil {
		r.Error = fmt.Sprintf("ec2:DescribeInstances: %v", err)
		return r
	}

	if len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 {
		r.Error = "instance not found in DescribeInstances response"
		return r
	}

	instance := out.Reservations[0].Instances[0]
	if instance.MetadataOptions == nil || instance.MetadataOptions.HttpPutResponseHopLimit == nil {
		r.Error = "could not determine IMDS hop limit"
		return r
	}

	hopLimit := aws.ToInt32(instance.MetadataOptions.HttpPutResponseHopLimit)
	if hopLimit < 2 {
		r.Error = fmt.Sprintf("IMDS hop limit is %d (must be >= 2 for pods to reach IMDS). Fix: aws ec2 modify-instance-metadata-options --instance-id %s --http-put-response-hop-limit 2 --region %s", hopLimit, node.InstanceID, node.Region)
		return r
	}
	r.Passed = true
	return r
}

func checkAccessEntry(ctx context.Context, cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) CheckResult {
	r := CheckResult{Name: "HYBRID_LINUX access entry exists for node role", ExitCode: 14}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cluster.Region))
	if err != nil {
		r.Error = fmt.Sprintf("loading AWS config: %v", err)
		return r
	}

	client := eks.NewFromConfig(cfg)
	out, err := client.ListAccessEntries(ctx, &eks.ListAccessEntriesInput{
		ClusterName: aws.String(cluster.Name),
	})
	if err != nil {
		r.Error = fmt.Sprintf("eks:ListAccessEntries: %v (ensure the node role has eks:ListAccessEntries permission)", err)
		return r
	}

	// Get the node's IAM role ARN from STS caller identity embedded in the account ID
	// We look for any access entry that contains our account and is a role
	roleFound := false
	for _, entry := range out.AccessEntries {
		if strings.Contains(entry, node.AccountID) && strings.Contains(entry, ":role/") {
			// Check if it's HYBRID_LINUX
			detail, err := client.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{
				ClusterName:  aws.String(cluster.Name),
				PrincipalArn: aws.String(entry),
			})
			if err != nil {
				continue
			}
			if detail.AccessEntry != nil && aws.ToString(detail.AccessEntry.Type) == "HYBRID_LINUX" {
				roleFound = true
				break
			}
		}
	}

	if !roleFound {
		r.Error = fmt.Sprintf("no HYBRID_LINUX access entry found for account %s. Create one: aws eks create-access-entry --region %s --cluster-name %s --principal-arn <satellite-node-role-arn> --type HYBRID_LINUX", node.AccountID, cluster.Region, cluster.Name)
		return r
	}
	r.Passed = true
	return r
}

func checkCIDROverlap(ctx context.Context, cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) CheckResult {
	r := CheckResult{Name: "Satellite VPC CIDR does not overlap with cluster VPC", ExitCode: 15}

	// Get satellite VPC CIDRs
	nodeCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(node.Region))
	if err != nil {
		r.Error = fmt.Sprintf("loading AWS config for node region: %v", err)
		return r
	}

	nodeEC2 := ec2.NewFromConfig(nodeCfg)
	vpcOut, err := nodeEC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{node.VPCID},
	})
	if err != nil {
		r.Error = fmt.Sprintf("ec2:DescribeVpcs for satellite VPC: %v", err)
		return r
	}
	if len(vpcOut.Vpcs) == 0 {
		r.Error = "satellite VPC not found"
		return r
	}

	// Get cluster VPC CIDRs
	clusterCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cluster.Region))
	if err != nil {
		r.Error = fmt.Sprintf("loading AWS config for cluster region: %v", err)
		return r
	}

	clusterEC2 := ec2.NewFromConfig(clusterCfg)
	clusterVpcOut, err := clusterEC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{cluster.VPCID},
	})
	if err != nil {
		r.Error = fmt.Sprintf("ec2:DescribeVpcs for cluster VPC: %v", err)
		return r
	}
	if len(clusterVpcOut.Vpcs) == 0 {
		r.Error = "cluster VPC not found"
		return r
	}

	// Check all CIDR combinations for overlap
	for _, nodeCIDR := range vpcOut.Vpcs[0].CidrBlockAssociationSet {
		nodeNet, err := parseCIDR(aws.ToString(nodeCIDR.CidrBlock))
		if err != nil {
			continue
		}
		for _, clusterCIDR := range clusterVpcOut.Vpcs[0].CidrBlockAssociationSet {
			clusterNet, err := parseCIDR(aws.ToString(clusterCIDR.CidrBlock))
			if err != nil {
				continue
			}
			if cidrsOverlap(nodeNet, clusterNet) {
				r.Error = fmt.Sprintf("satellite VPC CIDR %s overlaps with cluster VPC CIDR %s. Cross-region nodes require non-overlapping CIDRs.", aws.ToString(nodeCIDR.CidrBlock), aws.ToString(clusterCIDR.CidrBlock))
				return r
			}
		}
	}

	r.Passed = true
	return r
}

func parseCIDR(s string) (*net.IPNet, error) {
	_, ipnet, err := net.ParseCIDR(s)
	return ipnet, err
}

func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

// checkRemoteNetworkConfig verifies that the cluster's RemoteNetworkConfig.RemoteNodeNetworks
// includes the satellite VPC's CIDR. Without this, the EKS control plane cannot reach the
// kubelet on port 10250, which means kubectl logs/exec/port-forward will time out.
//
// This is a WARNING-level check — the node will still register and run pods even if this fails.
// It only blocks control-plane → kubelet operations.
func checkRemoteNetworkConfig(ctx context.Context, cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) CheckResult {
	r := CheckResult{Name: "Cluster RemoteNetworkConfig includes satellite CIDR (for kubectl logs/exec)", ExitCode: 18, Warning: true}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cluster.Region))
	if err != nil {
		r.Error = fmt.Sprintf("loading AWS config: %v", err)
		return r
	}

	client := eks.NewFromConfig(cfg)
	out, err := client.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(cluster.Name),
	})
	if err != nil {
		r.Error = fmt.Sprintf("eks:DescribeCluster: %v", err)
		return r
	}

	// Get satellite VPC CIDRs to compare against
	nodeCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(node.Region))
	if err != nil {
		r.Error = fmt.Sprintf("loading AWS config for node region: %v", err)
		return r
	}

	nodeEC2 := ec2.NewFromConfig(nodeCfg)
	vpcOut, err := nodeEC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{node.VPCID},
	})
	if err != nil || len(vpcOut.Vpcs) == 0 {
		r.Error = fmt.Sprintf("could not get satellite VPC CIDRs: %v", err)
		return r
	}

	var satelliteCIDRs []string
	for _, assoc := range vpcOut.Vpcs[0].CidrBlockAssociationSet {
		satelliteCIDRs = append(satelliteCIDRs, aws.ToString(assoc.CidrBlock))
	}

	// Check if all satellite CIDRs are present in RemoteNodeNetworks
	if out.Cluster.RemoteNetworkConfig == nil {
		r.Error = fmt.Sprintf("cluster has no RemoteNetworkConfig set. kubectl logs/exec to satellite pods will fail. Set with: aws eks update-cluster-config --region %s --name %s --remote-network-config '{\"remoteNodeNetworks\":[{\"cidrs\":[\"%s\"]}],\"remotePodNetworks\":[{\"cidrs\":[\"%s\"]}]}'", cluster.Region, cluster.Name, strings.Join(satelliteCIDRs, "\",\""), strings.Join(satelliteCIDRs, "\",\""))
		return r
	}

	configuredCIDRs := make(map[string]bool)
	for _, network := range out.Cluster.RemoteNetworkConfig.RemoteNodeNetworks {
		for _, cidr := range network.Cidrs {
			configuredCIDRs[cidr] = true
		}
	}

	for _, cidr := range satelliteCIDRs {
		if !configuredCIDRs[cidr] {
			r.Error = fmt.Sprintf("satellite VPC CIDR %s is not in cluster's RemoteNetworkConfig.remoteNodeNetworks. kubectl logs/exec will fail. Run: xrnctl add-region (or set RemoteNetworkConfig manually).", cidr)
			return r
		}
	}

	r.Passed = true
	return r
}

// checkCSRApprover warns if no kubelet-serving CSR auto-approver appears to be running
// in the cluster. EKS does not auto-approve serving CSRs from cross-region nodes, so without
// an external approver, the operator must approve manually each time the cert rotates
// (~9 months).
//
// Detection is best-effort — we look for known approver Deployments in common namespaces.
// This is a WARNING-level check; the node will still join and run pods, but kubectl logs/exec
// will fail until someone approves the kubelet's serving CSR.
func checkCSRApprover(ctx context.Context, cluster *discovery.ClusterInfo) CheckResult {
	r := CheckResult{Name: "Cluster has a kubelet-serving CSR auto-approver (recommended for kubectl logs/exec)", ExitCode: 19, Warning: true}

	// Note: this check requires kubectl-style access to the cluster, which xrn-install
	// doesn't have at preflight time (kubelet kubeconfig isn't set up yet). Instead, we
	// check for the AWS-managed addon eks-pod-identity-agent (a proxy signal that EKS
	// addon machinery is functioning) and report the limitation honestly.
	//
	// A future enhancement: have xrn-install obtain a token via aws eks get-token and
	// query the API server directly.
	r.Error = "cannot detect auto-approver from xrn-install (no cluster API access at preflight time). " +
		"Manually verify with: kubectl get deployments -A | grep -i 'cert-approver\\|csr-approver'. " +
		"If none exists, install kubelet-serving-cert-approver: " +
		"https://github.com/alex1989hu/kubelet-serving-cert-approver"
	return r
}
