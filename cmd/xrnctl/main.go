package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/eks-cross-region-nodes/pkg/registry"
)

// version is the default value when not set via -ldflags. Release builds should set this
// via `go build -ldflags="-X main.version=$(VERSION)"` (see Makefile).
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	ctx := context.Background()

	switch os.Args[1] {
	case "add-region":
		os.Exit(runAddRegion(ctx))
	case "remove-region":
		os.Exit(runRemoveRegion(ctx))
	case "list-regions":
		os.Exit(runListRegions(ctx))
	case "verify":
		os.Exit(runVerify(ctx))
	case "version":
		fmt.Printf("xrnctl %s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `xrnctl — Cross-region EKS cluster admin tool

Usage:
  xrnctl add-region     --cluster-name NAME --cluster-region REGION --vpc-id VPC --satellite-region REGION [--with-eniconfigs] [--subnet-ids s1,s2] [--security-group-ids sg1,sg2]
  xrnctl remove-region  --cluster-name NAME --cluster-region REGION --vpc-id VPC
  xrnctl list-regions   --cluster-name NAME --cluster-region REGION
  xrnctl verify         --cluster-name NAME --cluster-region REGION
  xrnctl version

Subcommands:
  add-region      Register a satellite VPC: updates aws-node-vpc-cidrs ConfigMap (and optionally creates ENIConfigs)
  remove-region   Deregister a satellite VPC (refuses if nodes still present)
  list-regions    Show all registered satellite VPCs
  verify          Check for drift between ConfigMap, ENIConfigs (if any), and actual nodes
  version         Print version

Notes:
  ENIConfigs (custom networking) are only needed when pods must use a different subnet
  or security group than the node. By default, add-region does NOT create ENIConfigs —
  the VPC CNI will allocate pod IPs from the node's primary subnet (auto-discovered
  via IMDS). Pass --with-eniconfigs to enable custom networking when it is genuinely
  needed (e.g., pods need a secondary CIDR or different security groups).
`)
}

type globalConfig struct {
	ClusterName   string
	ClusterRegion string
}

type addRegionConfig struct {
	globalConfig
	VPCID            string
	SatelliteRegion  string
	SubnetIDs        []string
	SecurityGroupIDs []string
	WithENIConfigs   bool
}

type removeRegionConfig struct {
	globalConfig
	VPCID string
}

func runAddRegion(ctx context.Context) int {
	cfg, err := parseAddRegionFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	input := &registry.AddRegionInput{
		VPCID:            cfg.VPCID,
		SatelliteRegion:  cfg.SatelliteRegion,
		SubnetIDs:        cfg.SubnetIDs,
		SecurityGroupIDs: cfg.SecurityGroupIDs,
		WithENIConfigs:   cfg.WithENIConfigs,
	}

	result, err := mgr.AddRegion(ctx, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Printf("✓ Registered satellite VPC %s (region %s)\n", cfg.VPCID, cfg.SatelliteRegion)
	fmt.Printf("  CIDRs added to ConfigMap: %v\n", result.CIDRs)
	if cfg.WithENIConfigs {
		fmt.Printf("  ENIConfigs created: %v\n", result.ENIConfigNames)
		fmt.Println("  Note: custom networking must be enabled on the aws-node DaemonSet")
	} else {
		fmt.Println("  ENIConfigs: not created (default). Pods will use the node's subnet.")
		fmt.Println("  To use custom networking instead, re-run with --with-eniconfigs.")
	}
	switch result.RemoteNetworkUpdate {
	case "added":
		fmt.Println("  RemoteNetworkConfig: updated (cluster is reconciling, may take ~1 minute to return to ACTIVE)")
		fmt.Println("  WARNING: existing satellite Node objects may be deleted by EKS reconciliation.")
		fmt.Println("           Restart kubelet on each satellite node to re-register them.")
	case "already-set":
		fmt.Println("  RemoteNetworkConfig: already includes these CIDRs (no update)")
	case "skipped":
		fmt.Println("  RemoteNetworkConfig: NOT updated (see warning above). kubectl logs/exec to satellite pods will fail until you set it manually.")
	}
	return 0
}

func runRemoveRegion(ctx context.Context) int {
	cfg, err := parseRemoveRegionFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if err := mgr.RemoveRegion(ctx, cfg.VPCID); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Printf("✓ Removed satellite VPC %s\n", cfg.VPCID)
	return 0
}

func runListRegions(ctx context.Context) int {
	cfg, err := parseGlobalFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	regions, err := mgr.ListRegions(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if len(regions) == 0 {
		fmt.Println("No satellite regions registered.")
		return 0
	}

	fmt.Printf("%-20s %-15s %-20s %s\n", "VPC ID", "REGION", "CIDRs", "ENIConfigs")
	fmt.Printf("%-20s %-15s %-20s %s\n", "------", "------", "-----", "----------")
	for _, r := range regions {
		fmt.Printf("%-20s %-15s %-20s %v\n", r.VPCID, r.Region, r.CIDRs, r.ENIConfigNames)
	}
	return 0
}

func runVerify(ctx context.Context) int {
	cfg, err := parseGlobalFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	issues, err := mgr.Verify(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if len(issues) == 0 {
		fmt.Println("✓ No drift detected. ConfigMap, ENIConfigs, and nodes are consistent.")
		return 0
	}

	fmt.Printf("Found %d issue(s):\n", len(issues))
	for _, issue := range issues {
		fmt.Printf("  ✗ %s\n", issue)
	}
	return 1
}

func parseGlobalFlags() (*globalConfig, error) {
	cfg := &globalConfig{}
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cluster-name":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--cluster-name requires a value")
			}
			i++
			cfg.ClusterName = args[i]
		case "--cluster-region":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--cluster-region requires a value")
			}
			i++
			cfg.ClusterRegion = args[i]
		}
	}
	if cfg.ClusterName == "" {
		return nil, fmt.Errorf("--cluster-name is required")
	}
	if cfg.ClusterRegion == "" {
		return nil, fmt.Errorf("--cluster-region is required")
	}
	return cfg, nil
}

func parseAddRegionFlags() (*addRegionConfig, error) {
	cfg := &addRegionConfig{}
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cluster-name":
			i++
			cfg.ClusterName = args[i]
		case "--cluster-region":
			i++
			cfg.ClusterRegion = args[i]
		case "--vpc-id":
			i++
			cfg.VPCID = args[i]
		case "--satellite-region":
			i++
			cfg.SatelliteRegion = args[i]
		case "--subnet-ids":
			i++
			cfg.SubnetIDs = splitComma(args[i])
		case "--security-group-ids":
			i++
			cfg.SecurityGroupIDs = splitComma(args[i])
		case "--with-eniconfigs":
			cfg.WithENIConfigs = true
		}
	}
	if cfg.ClusterName == "" || cfg.ClusterRegion == "" {
		return nil, fmt.Errorf("--cluster-name and --cluster-region are required")
	}
	if cfg.VPCID == "" {
		return nil, fmt.Errorf("--vpc-id is required")
	}
	if cfg.SatelliteRegion == "" {
		return nil, fmt.Errorf("--satellite-region is required")
	}
	if cfg.WithENIConfigs && len(cfg.SecurityGroupIDs) == 0 {
		return nil, fmt.Errorf("--security-group-ids is required when --with-eniconfigs is set")
	}
	return cfg, nil
}

func parseRemoveRegionFlags() (*removeRegionConfig, error) {
	cfg := &removeRegionConfig{}
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cluster-name":
			i++
			cfg.ClusterName = args[i]
		case "--cluster-region":
			i++
			cfg.ClusterRegion = args[i]
		case "--vpc-id":
			i++
			cfg.VPCID = args[i]
		}
	}
	if cfg.ClusterName == "" || cfg.ClusterRegion == "" {
		return nil, fmt.Errorf("--cluster-name and --cluster-region are required")
	}
	if cfg.VPCID == "" {
		return nil, fmt.Errorf("--vpc-id is required")
	}
	return cfg, nil
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	parts := make([]string, 0)
	for _, p := range split(s, ',') {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func split(s string, sep byte) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}
