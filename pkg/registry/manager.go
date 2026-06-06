package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/eks-cross-region-nodes/pkg/satellite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	configMapName      = "aws-node-vpc-cidrs"
	configMapNamespace = "kube-system"
	cidrDataKey        = "exclude-snat-cidrs"
	registryDataKey    = "registry.json"
	eniConfigAPIGroup  = "crd.k8s.amazonaws.com"
	eniConfigVersion   = "v1alpha1"
	eniConfigResource  = "eniconfigs"
)

var eniConfigGVR = schema.GroupVersionResource{
	Group:    eniConfigAPIGroup,
	Version:  eniConfigVersion,
	Resource: eniConfigResource,
}

type Manager struct {
	clusterName   string
	clusterRegion string
	profile       string
	k8sClient     kubernetes.Interface
	dynClient     dynamic.Interface
	eksClient     *eks.Client
}

type AddRegionInput struct {
	VPCID            string
	SatelliteRegion  string
	AccountID        string // AWS account the satellite VPC lives in; empty = cluster's own account
	SubnetIDs        []string
	SecurityGroupIDs []string
	WithENIConfigs   bool
	DryRun           bool // if true, render the cross-account satellite DS but don't apply it
}

type AddRegionResult struct {
	CIDRs               []string
	ENIConfigNames      []string
	RemoteNetworkUpdate string // "added", "already-set", or "skipped"
	CrossAccount        bool   // true if the satellite is in a different account than the cluster
	AlreadyRegistered   bool   // true if this VPC was already in the registry (idempotent re-run)
	SatelliteDS         string // name of the rendered/applied aws-node-satellite DaemonSet (cross-account only)
	SatelliteManifest   string // rendered manifest YAML, populated on DryRun
}

type SatelliteRegion struct {
	VPCID          string   `json:"vpc_id"`
	Region         string   `json:"region"`
	AccountID      string   `json:"account_id,omitempty"`
	CIDRs          []string `json:"vpc_cidrs"`
	ENIConfigNames []string `json:"eniconfig_names,omitempty"`
	AddedAt        string   `json:"added_at"`
}

type RegistryData struct {
	Version    int               `json:"version"`
	Satellites []SatelliteRegion `json:"satellites"`
}

// NewManager builds a Manager. profile selects a named AWS profile from the shared
// config/credentials files; pass "" to use the default credential chain.
func NewManager(ctx context.Context, clusterName, clusterRegion, profile string) (*Manager, error) {
	// Load kubeconfig
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	awsCfg, err := loadAWSConfig(ctx, clusterRegion, profile)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Manager{
		clusterName:   clusterName,
		clusterRegion: clusterRegion,
		profile:       profile,
		k8sClient:     k8sClient,
		dynClient:     dynClient,
		eksClient:     eks.NewFromConfig(awsCfg),
	}, nil
}

// loadAWSConfig loads an AWS config for the given region, optionally using a named profile.
func loadAWSConfig(ctx context.Context, region, profile string) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

func (m *Manager) AddRegion(ctx context.Context, input *AddRegionInput) (*AddRegionResult, error) {
	// 1. Discover satellite VPC CIDRs
	cidrs, err := m.getVPCCIDRs(ctx, input.VPCID, input.SatelliteRegion)
	if err != nil {
		return nil, fmt.Errorf("getting VPC CIDRs: %w", err)
	}

	// Idempotency: if this VPC is already registered, this is a re-run. Skip the overlap
	// check and ConfigMap rewrite (the VPC's own CIDRs are already in the exclude list and
	// would otherwise look like a self-overlap). Still re-attempt RemoteNetworkConfig in
	// case a prior run failed that step, then report.
	alreadyRegistered, err := m.isVPCRegistered(ctx, input.VPCID)
	if err != nil {
		return nil, fmt.Errorf("checking existing registration: %w", err)
	}
	if alreadyRegistered {
		remoteNetworkStatus, rerr := m.updateRemoteNetworkConfig(ctx, cidrs)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "  warning: failed to update RemoteNetworkConfig: %v\n", rerr)
			remoteNetworkStatus = "skipped"
		}
		crossAccount, _ := m.isCrossAccount(ctx, input.AccountID)
		return &AddRegionResult{
			CIDRs:               cidrs,
			RemoteNetworkUpdate: remoteNetworkStatus,
			CrossAccount:        crossAccount,
			AlreadyRegistered:   true,
		}, nil
	}

	// 2. Check for CIDR overlap with existing entries
	existing, err := m.getExistingCIDRs(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading existing CIDRs: %w", err)
	}
	if err := checkOverlap(cidrs, existing); err != nil {
		return nil, err
	}

	// 3. Optionally create ENIConfigs (only when custom networking is required)
	var eniConfigNames []string
	if input.WithENIConfigs {
		subnets, err := m.resolveSubnets(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("resolving subnets: %w", err)
		}
		eniConfigNames, err = m.createENIConfigs(ctx, subnets, input.SecurityGroupIDs)
		if err != nil {
			return nil, fmt.Errorf("creating ENIConfigs: %w", err)
		}
	}

	// 4. Update ConfigMap
	sat := SatelliteRegion{
		VPCID:          input.VPCID,
		Region:         input.SatelliteRegion,
		AccountID:      input.AccountID,
		CIDRs:          cidrs,
		ENIConfigNames: eniConfigNames,
		AddedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.updateConfigMap(ctx, sat, true); err != nil {
		return nil, fmt.Errorf("updating ConfigMap: %w", err)
	}

	// 5. Update cluster RemoteNetworkConfig so the control plane can reach the kubelet
	remoteNetworkStatus, err := m.updateRemoteNetworkConfig(ctx, cidrs)
	if err != nil {
		// Don't fail the whole operation — RemoteNetworkConfig is a best-effort update.
		// The operator can set it manually if this fails.
		fmt.Fprintf(os.Stderr, "  warning: failed to update RemoteNetworkConfig: %v\n", err)
		remoteNetworkStatus = "skipped"
	}

	// 6. Determine whether this satellite is cross-account. Same-account satellites ride
	// the cluster's existing aws-node DaemonSet; cross-account satellites need their own.
	crossAccount, err := m.isCrossAccount(ctx, input.AccountID)
	if err != nil {
		// Non-fatal: default to same-account (the conservative interpretation — no extra DS).
		fmt.Fprintf(os.Stderr, "  warning: could not determine cluster account, assuming same-account: %v\n", err)
		crossAccount = false
	}

	result := &AddRegionResult{
		CIDRs:               cidrs,
		ENIConfigNames:      eniConfigNames,
		RemoteNetworkUpdate: remoteNetworkStatus,
		CrossAccount:        crossAccount,
	}

	// 7. Cross-account only: render (and apply, unless dry-run) a dedicated aws-node-satellite
	// DaemonSet. Stock aws-node already excludes compute-type=hybrid nodes, so no patch to it
	// is needed. Same-account satellites ride stock aws-node and skip this entirely.
	if crossAccount {
		manifest, derr := satellite.Render(satellite.Params{
			AccountID:        input.AccountID,
			Region:           input.SatelliteRegion,
			ClusterName:      m.clusterName,
			ExcludeSNATCIDRs: m.allExcludeSNATCIDRs(ctx),
		})
		if derr != nil {
			return nil, fmt.Errorf("rendering satellite DaemonSet: %w", derr)
		}
		result.SatelliteDS = satellite.Params{AccountID: input.AccountID, Region: input.SatelliteRegion}.Name()
		if input.DryRun {
			result.SatelliteManifest = manifest
		} else if aerr := m.applyManifest(ctx, manifest); aerr != nil {
			return nil, fmt.Errorf("applying satellite DaemonSet: %w", aerr)
		}
	}

	return result, nil
}

// allExcludeSNATCIDRs returns the full SNAT-exclusion CIDR set (cluster + all registered
// satellites) currently in the ConfigMap, for baking into the satellite DS env. Best-effort:
// returns whatever it can read.
func (m *Manager) allExcludeSNATCIDRs(ctx context.Context) []string {
	cidrs, _ := m.getExistingCIDRs(ctx)
	return cidrs
}

// manifestGVRs maps the kinds emitted by pkg/satellite to their GroupVersionResource and
// whether they're namespaced. We only ever apply these three kinds.
var manifestGVRs = map[string]struct {
	gvr        schema.GroupVersionResource
	namespaced bool
}{
	"ServiceAccount":     {schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}, true},
	"DaemonSet":          {schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, true},
	"ClusterRoleBinding": {schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, false},
}

// applyManifest decodes a multi-document YAML manifest and creates or updates each object
// via the dynamic client. Idempotent: existing objects are updated (preserving
// resourceVersion), missing ones are created.
func (m *Manager) applyManifest(ctx context.Context, manifest string) error {
	for _, doc := range strings.Split(manifest, "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			return fmt.Errorf("decoding manifest document: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}

		kind := obj.GetKind()
		mapping, ok := manifestGVRs[kind]
		if !ok {
			return fmt.Errorf("unsupported kind in satellite manifest: %q", kind)
		}

		var ri dynamic.ResourceInterface
		if mapping.namespaced {
			ns := obj.GetNamespace()
			if ns == "" {
				ns = configMapNamespace
			}
			ri = m.dynClient.Resource(mapping.gvr).Namespace(ns)
		} else {
			ri = m.dynClient.Resource(mapping.gvr)
		}

		existing, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, cerr := ri.Create(ctx, obj, metav1.CreateOptions{}); cerr != nil {
				return fmt.Errorf("creating %s/%s: %w", kind, obj.GetName(), cerr)
			}
			continue
		} else if err != nil {
			return fmt.Errorf("getting %s/%s: %w", kind, obj.GetName(), err)
		}

		// Update: carry over resourceVersion so the update is accepted.
		obj.SetResourceVersion(existing.GetResourceVersion())
		if _, uerr := ri.Update(ctx, obj, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("updating %s/%s: %w", kind, obj.GetName(), uerr)
		}
	}
	return nil
}

// isVPCRegistered reports whether vpcID is already present in the registry.
func (m *Manager) isVPCRegistered(ctx context.Context, vpcID string) (bool, error) {
	reg, err := m.getRegistry(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range reg.Satellites {
		if s.VPCID == vpcID {
			return true, nil
		}
	}
	return false, nil
}

// isCrossAccount reports whether satelliteAccountID differs from the cluster's account.
// An empty satelliteAccountID means "same account as the cluster" (the caller didn't
// specify one), so it is never cross-account.
func (m *Manager) isCrossAccount(ctx context.Context, satelliteAccountID string) (bool, error) {
	if satelliteAccountID == "" {
		return false, nil
	}
	clusterAccountID, err := m.getClusterAccountID(ctx)
	if err != nil {
		return false, err
	}
	return satelliteAccountID != clusterAccountID, nil
}

// getClusterAccountID extracts the AWS account ID from the cluster ARN
// (arn:aws:eks:<region>:<account>:cluster/<name>).
func (m *Manager) getClusterAccountID(ctx context.Context) (string, error) {
	out, err := m.eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(m.clusterName),
	})
	if err != nil {
		return "", fmt.Errorf("eks:DescribeCluster: %w", err)
	}
	return accountIDFromARN(aws.ToString(out.Cluster.Arn))
}

// accountIDFromARN extracts the account field (index 4) from an ARN.
// arn:partition:service:region:account-id:resource → account-id.
func accountIDFromARN(arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 || parts[4] == "" {
		return "", fmt.Errorf("could not parse account ID from ARN %q", arn)
	}
	return parts[4], nil
}

// updateRemoteNetworkConfig adds the satellite CIDRs to the cluster's
// RemoteNetworkConfig.remoteNodeNetworks (required for the control plane to reach the
// kubelet on :10250). Returns "added" if the cluster was updated, "already-set" if all
// CIDRs were already present, or an error if the update failed.
//
// We deliberately manage ONLY remoteNodeNetworks. remotePodNetworks is optional and EKS
// rejects any CIDR that appears in both lists (InvalidParameterException: "overlaps with
// already existing CIDR"). Existing remotePodNetworks, if any, are preserved verbatim.
//
// IMPORTANT: Updating RemoteNetworkConfig on a running cluster causes EKS to delete existing
// satellite Node objects. This is documented in PRD §14. Operators must restart kubelet on
// affected nodes after this call.
func (m *Manager) updateRemoteNetworkConfig(ctx context.Context, newCIDRs []string) (string, error) {
	out, err := m.eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(m.clusterName),
	})
	if err != nil {
		return "", fmt.Errorf("eks:DescribeCluster: %w", err)
	}

	// Collect existing remote node CIDRs.
	var existingNodeCIDRs []string
	var existingPodNetworks []ekstypes.RemotePodNetwork
	if rnc := out.Cluster.RemoteNetworkConfig; rnc != nil {
		for _, network := range rnc.RemoteNodeNetworks {
			existingNodeCIDRs = append(existingNodeCIDRs, network.Cidrs...)
		}
		// Preserve existing pod networks as-is; we don't manage them.
		existingPodNetworks = rnc.RemotePodNetworks
	}

	mergedNodeCIDRs, changed := mergeRemoteNetworkCIDRs(existingNodeCIDRs, newCIDRs)
	if !changed {
		return "already-set", nil
	}

	req := &ekstypes.RemoteNetworkConfigRequest{
		RemoteNodeNetworks: []ekstypes.RemoteNodeNetwork{{Cidrs: mergedNodeCIDRs}},
	}
	// Echo back existing pod networks unchanged so the update doesn't drop them.
	if len(existingPodNetworks) > 0 {
		req.RemotePodNetworks = existingPodNetworks
	}

	_, err = m.eksClient.UpdateClusterConfig(ctx, &eks.UpdateClusterConfigInput{
		Name:                aws.String(m.clusterName),
		RemoteNetworkConfig: req,
	})
	if err != nil {
		return "", fmt.Errorf("eks:UpdateClusterConfig: %w", err)
	}

	return "added", nil
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// mergeRemoteNetworkCIDRs merges newCIDRs into existing, returning the merged sorted slice
// and whether any new CIDRs were added (false = all already present, no update needed).
// Pure function — extracted for testability.
func mergeRemoteNetworkCIDRs(existing, newCIDRs []string) (merged []string, changed bool) {
	set := make(map[string]bool)
	for _, c := range existing {
		set[c] = true
	}
	changed = false
	for _, c := range newCIDRs {
		if !set[c] {
			set[c] = true
			changed = true
		}
	}
	merged = mapKeys(set)
	sort.Strings(merged)
	return merged, changed
}

func (m *Manager) RemoveRegion(ctx context.Context, vpcID string) error {
	// Check for nodes still in this region
	nodes, err := m.getNodesInVPC(ctx, vpcID)
	if err != nil {
		return fmt.Errorf("checking for nodes: %w", err)
	}
	if len(nodes) > 0 {
		return fmt.Errorf("cannot remove VPC %s: %d node(s) still registered (%v). Drain and terminate them first", vpcID, len(nodes), nodes)
	}

	// Get registry to find ENIConfig names and CIDRs
	reg, err := m.getRegistry(ctx)
	if err != nil {
		return fmt.Errorf("reading registry: %w", err)
	}

	var target *SatelliteRegion
	for i, s := range reg.Satellites {
		if s.VPCID == vpcID {
			target = &reg.Satellites[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("VPC %s not found in registry", vpcID)
	}

	// Delete ENIConfigs
	for _, name := range target.ENIConfigNames {
		if err := m.deleteENIConfig(ctx, name); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: failed to delete ENIConfig %s: %v\n", name, err)
		}
	}

	// Update ConfigMap (remove this satellite)
	if err := m.updateConfigMap(ctx, *target, false); err != nil {
		return fmt.Errorf("updating ConfigMap: %w", err)
	}

	return nil
}

func (m *Manager) ListRegions(ctx context.Context) ([]SatelliteRegion, error) {
	reg, err := m.getRegistry(ctx)
	if err != nil {
		return nil, err
	}
	return reg.Satellites, nil
}

func (m *Manager) Verify(ctx context.Context) ([]string, error) {
	var issues []string

	reg, err := m.getRegistry(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading registry: %w", err)
	}

	// Check each satellite's ENIConfigs exist
	for _, sat := range reg.Satellites {
		for _, name := range sat.ENIConfigNames {
			exists, err := m.eniConfigExists(ctx, name)
			if err != nil {
				issues = append(issues, fmt.Sprintf("Error checking ENIConfig %s: %v", name, err))
			} else if !exists {
				issues = append(issues, fmt.Sprintf("ENIConfig %s is in registry but does not exist in cluster (VPC %s, region %s)", name, sat.VPCID, sat.Region))
			}
		}
	}

	// Check ConfigMap CIDRs match registry
	cmCIDRs, err := m.getExistingCIDRs(ctx)
	if err != nil {
		issues = append(issues, fmt.Sprintf("Cannot read ConfigMap: %v", err))
	} else {
		for _, sat := range reg.Satellites {
			for _, cidr := range sat.CIDRs {
				found := false
				for _, c := range cmCIDRs {
					if c == cidr {
						found = true
						break
					}
				}
				if !found {
					issues = append(issues, fmt.Sprintf("CIDR %s (VPC %s) is in registry but missing from ConfigMap exclude-snat-cidrs", cidr, sat.VPCID))
				}
			}
		}
	}

	// Check nodes with cross-region label have matching ENIConfigs
	nodeList, err := m.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "eks.amazonaws.com/compute-type=cross-region",
	})
	if err != nil {
		issues = append(issues, fmt.Sprintf("Cannot list nodes: %v", err))
	} else {
		for _, node := range nodeList.Items {
			zone := node.Labels["topology.kubernetes.io/zone"]
			if zone == "" {
				issues = append(issues, fmt.Sprintf("Node %s has compute-type=cross-region but no topology.kubernetes.io/zone label", node.Name))
				continue
			}
			exists, _ := m.eniConfigExists(ctx, zone)
			if !exists {
				issues = append(issues, fmt.Sprintf("Node %s is in zone %s but no ENIConfig named %s exists", node.Name, zone, zone))
			}
		}
	}

	return issues, nil
}

func (m *Manager) getVPCCIDRs(ctx context.Context, vpcID, region string) ([]string, error) {
	cfg, err := loadAWSConfig(ctx, region, m.profile)
	if err != nil {
		return nil, err
	}

	client := ec2.NewFromConfig(cfg)
	out, err := client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{
		VpcIds: []string{vpcID},
	})
	if err != nil {
		return nil, err
	}
	if len(out.Vpcs) == 0 {
		return nil, fmt.Errorf("VPC %s not found in region %s", vpcID, region)
	}

	var cidrs []string
	for _, assoc := range out.Vpcs[0].CidrBlockAssociationSet {
		if assoc.CidrBlockState != nil && assoc.CidrBlockState.State == ec2types.VpcCidrBlockStateCodeAssociated {
			cidrs = append(cidrs, aws.ToString(assoc.CidrBlock))
		}
	}
	return cidrs, nil
}

func (m *Manager) getExistingCIDRs(ctx context.Context) ([]string, error) {
	cm, err := m.k8sClient.CoreV1().ConfigMaps(configMapNamespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, nil // ConfigMap doesn't exist yet — no existing CIDRs
	}
	raw := cm.Data[cidrDataKey]
	if raw == "" {
		return nil, nil
	}
	var cidrs []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			cidrs = append(cidrs, line)
		}
	}
	return cidrs, nil
}

func (m *Manager) getRegistry(ctx context.Context) (*RegistryData, error) {
	cm, err := m.k8sClient.CoreV1().ConfigMaps(configMapNamespace).Get(ctx, configMapName, metav1.GetOptions{})
	if err != nil {
		return &RegistryData{Version: 1}, nil
	}
	raw := cm.Data[registryDataKey]
	if raw == "" {
		return &RegistryData{Version: 1}, nil
	}
	var reg RegistryData
	if err := json.Unmarshal([]byte(raw), &reg); err != nil {
		return nil, fmt.Errorf("parsing registry.json: %w", err)
	}
	return &reg, nil
}

func (m *Manager) resolveSubnets(ctx context.Context, input *AddRegionInput) ([]subnetInfo, error) {
	if len(input.SubnetIDs) > 0 {
		return m.describeSubnets(ctx, input.SubnetIDs, input.SatelliteRegion)
	}

	// Auto-discover: find all private subnets in the VPC
	cfg, err := loadAWSConfig(ctx, input.SatelliteRegion, m.profile)
	if err != nil {
		return nil, err
	}

	client := ec2.NewFromConfig(cfg)
	out, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("vpc-id"), Values: []string{input.VPCID}},
		},
	})
	if err != nil {
		return nil, err
	}

	var subnets []subnetInfo
	for _, s := range out.Subnets {
		// Skip public subnets (those with MapPublicIpOnLaunch)
		if aws.ToBool(s.MapPublicIpOnLaunch) {
			continue
		}
		subnets = append(subnets, subnetInfo{
			ID: aws.ToString(s.SubnetId),
			AZ: aws.ToString(s.AvailabilityZone),
		})
	}

	if len(subnets) == 0 {
		return nil, fmt.Errorf("no private subnets found in VPC %s. Specify --subnet-ids explicitly", input.VPCID)
	}
	return subnets, nil
}

func (m *Manager) describeSubnets(ctx context.Context, subnetIDs []string, region string) ([]subnetInfo, error) {
	cfg, err := loadAWSConfig(ctx, region, m.profile)
	if err != nil {
		return nil, err
	}

	client := ec2.NewFromConfig(cfg)
	out, err := client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: subnetIDs,
	})
	if err != nil {
		return nil, err
	}

	var subnets []subnetInfo
	for _, s := range out.Subnets {
		subnets = append(subnets, subnetInfo{
			ID: aws.ToString(s.SubnetId),
			AZ: aws.ToString(s.AvailabilityZone),
		})
	}
	return subnets, nil
}

type subnetInfo struct {
	ID string
	AZ string
}

func (m *Manager) createENIConfigs(ctx context.Context, subnets []subnetInfo, sgIDs []string) ([]string, error) {
	var names []string

	for _, subnet := range subnets {
		name := subnet.AZ // ENIConfig name = AZ name

		sgList := make([]interface{}, len(sgIDs))
		for i, sg := range sgIDs {
			sgList[i] = sg
		}

		obj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": eniConfigAPIGroup + "/" + eniConfigVersion,
				"kind":       "ENIConfig",
				"metadata": map[string]interface{}{
					"name": name,
				},
				"spec": map[string]interface{}{
					"subnet":         subnet.ID,
					"securityGroups": sgList,
				},
			},
		}

		_, err := m.dynClient.Resource(eniConfigGVR).Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			if strings.Contains(err.Error(), "already exists") {
				fmt.Printf("  ENIConfig %s already exists, skipping\n", name)
			} else {
				return nil, fmt.Errorf("creating ENIConfig %s: %w", name, err)
			}
		} else {
			fmt.Printf("  Created ENIConfig %s (subnet %s, AZ %s)\n", name, subnet.ID, subnet.AZ)
		}
		names = append(names, name)
	}

	return names, nil
}

func (m *Manager) deleteENIConfig(ctx context.Context, name string) error {
	return m.dynClient.Resource(eniConfigGVR).Delete(ctx, name, metav1.DeleteOptions{})
}

func (m *Manager) eniConfigExists(ctx context.Context, name string) (bool, error) {
	_, err := m.dynClient.Resource(eniConfigGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (m *Manager) updateConfigMap(ctx context.Context, satellite SatelliteRegion, adding bool) error {
	// Get or create ConfigMap
	cm, err := m.k8sClient.CoreV1().ConfigMaps(configMapNamespace).Get(ctx, configMapName, metav1.GetOptions{})
	creating := false
	if err != nil {
		creating = true
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: configMapNamespace,
				Annotations: map[string]string{
					"eks.amazonaws.com/managed-by": "xrnctl",
				},
			},
			Data: map[string]string{},
		}
	}

	// Update registry.json
	var reg RegistryData
	if raw := cm.Data[registryDataKey]; raw != "" {
		json.Unmarshal([]byte(raw), &reg)
	}
	reg.Version = 1

	if adding {
		// Check if already present
		for _, s := range reg.Satellites {
			if s.VPCID == satellite.VPCID {
				return fmt.Errorf("VPC %s is already registered", satellite.VPCID)
			}
		}
		reg.Satellites = append(reg.Satellites, satellite)
	} else {
		var updated []SatelliteRegion
		for _, s := range reg.Satellites {
			if s.VPCID != satellite.VPCID {
				updated = append(updated, s)
			}
		}
		reg.Satellites = updated
	}

	regJSON, _ := json.MarshalIndent(reg, "", "  ")
	cm.Data[registryDataKey] = string(regJSON)

	// Rebuild exclude-snat-cidrs from all satellites
	var allCIDRs []string
	for _, s := range reg.Satellites {
		allCIDRs = append(allCIDRs, s.CIDRs...)
	}

	// Also include the cluster VPC CIDRs
	clusterCIDRs, _ := m.getClusterVPCCIDRs(ctx)
	allCIDRs = append(allCIDRs, clusterCIDRs...)

	sort.Strings(allCIDRs)
	allCIDRs = dedupe(allCIDRs)
	cm.Data[cidrDataKey] = strings.Join(allCIDRs, "\n")

	if creating {
		_, err = m.k8sClient.CoreV1().ConfigMaps(configMapNamespace).Create(ctx, cm, metav1.CreateOptions{})
	} else {
		_, err = m.k8sClient.CoreV1().ConfigMaps(configMapNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

func (m *Manager) getClusterVPCCIDRs(ctx context.Context) ([]string, error) {
	out, err := m.eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(m.clusterName),
	})
	if err != nil {
		return nil, err
	}

	vpcID := aws.ToString(out.Cluster.ResourcesVpcConfig.VpcId)
	cfg, err := loadAWSConfig(ctx, m.clusterRegion, m.profile)
	if err != nil {
		return nil, err
	}

	ec2Client := ec2.NewFromConfig(cfg)
	vpcOut, err := ec2Client.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil {
		return nil, err
	}
	if len(vpcOut.Vpcs) == 0 {
		return nil, nil
	}

	var cidrs []string
	for _, assoc := range vpcOut.Vpcs[0].CidrBlockAssociationSet {
		if assoc.CidrBlockState != nil && assoc.CidrBlockState.State == ec2types.VpcCidrBlockStateCodeAssociated {
			cidrs = append(cidrs, aws.ToString(assoc.CidrBlock))
		}
	}
	return cidrs, nil
}

func (m *Manager) getNodesInVPC(ctx context.Context, vpcID string) ([]string, error) {
	// We can't directly query by VPC, but we can look for cross-region nodes
	// and check their region matches the satellite
	reg, err := m.getRegistry(ctx)
	if err != nil {
		return nil, err
	}

	var targetRegion string
	for _, s := range reg.Satellites {
		if s.VPCID == vpcID {
			targetRegion = s.Region
			break
		}
	}
	if targetRegion == "" {
		return nil, nil
	}

	nodeList, err := m.k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("eks.amazonaws.com/compute-type=cross-region,topology.kubernetes.io/region=%s", targetRegion),
	})
	if err != nil {
		return nil, err
	}

	var names []string
	for _, n := range nodeList.Items {
		names = append(names, n.Name)
	}
	return names, nil
}

func checkOverlap(newCIDRs, existingCIDRs []string) error {
	for _, newCIDR := range newCIDRs {
		_, newNet, err := net.ParseCIDR(newCIDR)
		if err != nil {
			continue
		}
		for _, existingCIDR := range existingCIDRs {
			_, existingNet, err := net.ParseCIDR(existingCIDR)
			if err != nil {
				continue
			}
			if newNet.Contains(existingNet.IP) || existingNet.Contains(newNet.IP) {
				return fmt.Errorf("CIDR %s overlaps with existing CIDR %s. Cross-region nodes require non-overlapping CIDRs", newCIDR, existingCIDR)
			}
		}
	}
	return nil
}

func dedupe(s []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	return result
}
