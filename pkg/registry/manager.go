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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	k8sClient     kubernetes.Interface
	dynClient     dynamic.Interface
	eksClient     *eks.Client
}

type AddRegionInput struct {
	VPCID            string
	SatelliteRegion  string
	SubnetIDs        []string
	SecurityGroupIDs []string
}

type AddRegionResult struct {
	CIDRs          []string
	ENIConfigNames []string
}

type SatelliteRegion struct {
	VPCID          string   `json:"vpc_id"`
	Region         string   `json:"region"`
	CIDRs          []string `json:"vpc_cidrs"`
	ENIConfigNames []string `json:"eniconfig_names,omitempty"`
	AddedAt        string   `json:"added_at"`
}

type RegistryData struct {
	Version    int               `json:"version"`
	Satellites []SatelliteRegion `json:"satellites"`
}

func NewManager(ctx context.Context, clusterName, clusterRegion string) (*Manager, error) {
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

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(clusterRegion))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &Manager{
		clusterName:   clusterName,
		clusterRegion: clusterRegion,
		k8sClient:     k8sClient,
		dynClient:     dynClient,
		eksClient:     eks.NewFromConfig(awsCfg),
	}, nil
}

func (m *Manager) AddRegion(ctx context.Context, input *AddRegionInput) (*AddRegionResult, error) {
	// 1. Discover satellite VPC CIDRs
	cidrs, err := m.getVPCCIDRs(ctx, input.VPCID, input.SatelliteRegion)
	if err != nil {
		return nil, fmt.Errorf("getting VPC CIDRs: %w", err)
	}

	// 2. Check for CIDR overlap with existing entries
	existing, err := m.getExistingCIDRs(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading existing CIDRs: %w", err)
	}
	if err := checkOverlap(cidrs, existing); err != nil {
		return nil, err
	}

	// 3. Discover subnets if not provided
	subnets, err := m.resolveSubnets(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("resolving subnets: %w", err)
	}

	// 4. Create ENIConfigs
	eniConfigNames, err := m.createENIConfigs(ctx, subnets, input.SecurityGroupIDs)
	if err != nil {
		return nil, fmt.Errorf("creating ENIConfigs: %w", err)
	}

	// 5. Update ConfigMap
	satellite := SatelliteRegion{
		VPCID:          input.VPCID,
		Region:         input.SatelliteRegion,
		CIDRs:          cidrs,
		ENIConfigNames: eniConfigNames,
		AddedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.updateConfigMap(ctx, satellite, true); err != nil {
		return nil, fmt.Errorf("updating ConfigMap: %w", err)
	}

	return &AddRegionResult{
		CIDRs:          cidrs,
		ENIConfigNames: eniConfigNames,
	}, nil
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
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
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
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(input.SatelliteRegion))
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
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
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
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(m.clusterRegion))
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
