package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/eks-cross-region-nodes/pkg/registry"
)

const version = "0.1.0"

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
  xrnctl add-region     --cluster-name NAME --cluster-region REGION --vpc-id VPC --satellite-region REGION [--subnet-ids s1,s2] [--security-group-ids sg1,sg2]
  xrnctl remove-region  --cluster-name NAME --cluster-region REGION --vpc-id VPC
  xrnctl list-regions   --cluster-name NAME --cluster-region REGION
  xrnctl verify         --cluster-name NAME --cluster-region REGION
  xrnctl version

Subcommands:
  add-region      Register a satellite VPC: creates ENIConfigs, updates aws-node-vpc-cidrs ConfigMap
  remove-region   Deregister a satellite VPC (refuses if nodes still present)
  list-regions    Show all registered satellite VPCs
  verify          Check for drift between ConfigMap, ENIConfigs, and actual nodes
  version         Print version
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
	}

	result, err := mgr.AddRegion(ctx, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Printf("✓ Registered satellite VPC %s (region %s)\n", cfg.VPCID, cfg.SatelliteRegion)
	fmt.Printf("  CIDRs added to ConfigMap: %v\n", result.CIDRs)
	fmt.Printf("  ENIConfigs created: %v\n", result.ENIConfigNames)
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
