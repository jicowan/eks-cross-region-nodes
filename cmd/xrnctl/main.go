package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/eks-cross-region-nodes/pkg/iamsetup"
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
	case "add-satellite":
		os.Exit(runAddSatellite(ctx))
	case "remove-satellite":
		os.Exit(runRemoveSatellite(ctx))
	case "list-satellites":
		os.Exit(runListSatellites(ctx))
	case "setup-iam":
		os.Exit(runSetupIAM(ctx))
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
	fmt.Fprintf(os.Stderr, `xrnctl — Cross-region / cross-account EKS cluster admin tool

Usage:
  xrnctl add-satellite     --cluster-name NAME --cluster-region REGION --vpc-id VPC --satellite-region REGION [--account-id ACCT] [--with-eniconfigs] [--subnet-ids s1,s2] [--security-group-ids sg1,sg2]
  xrnctl remove-satellite  --cluster-name NAME --cluster-region REGION --vpc-id VPC
  xrnctl list-satellites   --cluster-name NAME --cluster-region REGION
  xrnctl setup-iam         --cluster-name NAME --cluster-region REGION [--node-role-name NAME] [--node-role-arn ARN] [--node-role-only|--access-entry-only] [--profile PROFILE]
  xrnctl verify            --cluster-name NAME --cluster-region REGION
  xrnctl version

Global flags (all subcommands):
  --profile PROFILE   Use a named AWS profile from ~/.aws/config instead of the default
                      credential chain. Essential for cross-account: run setup-iam with the
                      satellite-account profile to create the node role, then with the
                      cluster-account profile to create the access entry.

Subcommands:
  add-satellite     Register a satellite VPC: updates aws-node-vpc-cidrs ConfigMap (and optionally creates ENIConfigs)
  remove-satellite  Deregister a satellite VPC (refuses if nodes still present)
  list-satellites   Show all registered satellite VPCs
  setup-iam         Create the node IAM role + instance profile and/or the HYBRID_LINUX access entry
  verify            Check for drift between ConfigMap, ENIConfigs (if any), and actual nodes
  version           Print version

Notes:
  --account-id identifies the AWS account the satellite VPC lives in. Omit it (or set it
  to the cluster's own account) for same-account / cross-region satellites — those ride the
  cluster's existing aws-node DaemonSet. Set it to a DIFFERENT account for cross-account
  satellites, which require their own aws-node DaemonSet (handled separately).

  setup-iam (same-account): run once (default profile) — creates the node role + instance
  profile AND the HYBRID_LINUX access entry in one go.
  setup-iam (cross-account): run twice —
    1) --profile <satellite> --node-role-name CrossRegionNodeRole --node-role-only
    2) --profile <cluster>   --node-role-arn <arn-from-step-1> --access-entry-only

  ENIConfigs (custom networking) are only needed when pods must use a different subnet
  or security group than the node. By default, add-satellite does NOT create ENIConfigs —
  the VPC CNI will allocate pod IPs from the node's primary subnet (auto-discovered
  via IMDS). Pass --with-eniconfigs to enable custom networking when it is genuinely
  needed (e.g., pods need a secondary CIDR or different security groups).
`)
}

type globalConfig struct {
	ClusterName   string
	ClusterRegion string
	Profile       string
}

type addSatelliteConfig struct {
	globalConfig
	VPCID            string
	SatelliteRegion  string
	AccountID        string
	SubnetIDs        []string
	SecurityGroupIDs []string
	WithENIConfigs   bool
}

type removeSatelliteConfig struct {
	globalConfig
	VPCID string
}

func runAddSatellite(ctx context.Context) int {
	cfg, err := parseAddSatelliteFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion, cfg.Profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	input := &registry.AddRegionInput{
		VPCID:            cfg.VPCID,
		SatelliteRegion:  cfg.SatelliteRegion,
		AccountID:        cfg.AccountID,
		SubnetIDs:        cfg.SubnetIDs,
		SecurityGroupIDs: cfg.SecurityGroupIDs,
		WithENIConfigs:   cfg.WithENIConfigs,
	}

	result, err := mgr.AddRegion(ctx, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if result.AlreadyRegistered {
		fmt.Printf("✓ Satellite VPC %s (region %s) is already registered — no changes to ConfigMap\n", cfg.VPCID, cfg.SatelliteRegion)
	} else {
		fmt.Printf("✓ Registered satellite VPC %s (region %s)\n", cfg.VPCID, cfg.SatelliteRegion)
	}
	fmt.Printf("  CIDRs: %v\n", result.CIDRs)
	if result.CrossAccount {
		fmt.Printf("  Account: %s (cross-account — requires a dedicated aws-node DaemonSet)\n", cfg.AccountID)
	} else {
		fmt.Println("  Account: same as cluster (rides the existing aws-node DaemonSet)")
	}
	if !result.AlreadyRegistered {
		if cfg.WithENIConfigs {
			fmt.Printf("  ENIConfigs created: %v\n", result.ENIConfigNames)
			fmt.Println("  Note: custom networking must be enabled on the aws-node DaemonSet")
		} else {
			fmt.Println("  ENIConfigs: not created (default). Pods will use the node's subnet.")
			fmt.Println("  To use custom networking instead, re-run with --with-eniconfigs.")
		}
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

func runRemoveSatellite(ctx context.Context) int {
	cfg, err := parseRemoveSatelliteFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion, cfg.Profile)
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

func runListSatellites(ctx context.Context) int {
	cfg, err := parseGlobalFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion, cfg.Profile)
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

	fmt.Printf("%-22s %-15s %-15s %-22s %s\n", "VPC ID", "REGION", "ACCOUNT", "CIDRs", "ENICONFIGS")
	fmt.Printf("%-22s %-15s %-15s %-22s %s\n", "------", "------", "-------", "-----", "----------")
	for _, r := range regions {
		account := r.AccountID
		if account == "" {
			account = "(cluster)"
		}
		eniConfigs := "-"
		if len(r.ENIConfigNames) > 0 {
			eniConfigs = strings.Join(r.ENIConfigNames, ",")
		}
		fmt.Printf("%-22s %-15s %-15s %-22s %s\n", r.VPCID, r.Region, account, strings.Join(r.CIDRs, ","), eniConfigs)
	}
	return 0
}

type setupIAMConfig struct {
	globalConfig
	NodeRoleName    string
	NodeRoleARN     string
	NodeRoleOnly    bool
	AccessEntryOnly bool
}

func runSetupIAM(ctx context.Context) int {
	cfg, err := parseSetupIAMFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	res, err := iamsetup.Run(ctx, iamsetup.Options{
		ClusterName:     cfg.ClusterName,
		ClusterRegion:   cfg.ClusterRegion,
		Profile:         cfg.Profile,
		NodeRoleName:    cfg.NodeRoleName,
		NodeRoleARN:     cfg.NodeRoleARN,
		NodeRoleOnly:    cfg.NodeRoleOnly,
		AccessEntryOnly: cfg.AccessEntryOnly,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if res.RoleName != "" {
		verb := "exists"
		if res.RoleCreated {
			verb = "created"
		}
		fmt.Printf("✓ Node role %s (%s)\n", res.RoleARN, verb)
	}
	if res.InstanceProfileName != "" {
		verb := "exists"
		if res.InstanceProfileMade {
			verb = "created"
		}
		fmt.Printf("✓ Instance profile %s (%s)\n", res.InstanceProfileName, verb)
	}
	if res.AccessEntryARN != "" {
		verb := "exists"
		if res.AccessEntryCreated {
			verb = "created"
		}
		fmt.Printf("✓ HYBRID_LINUX access entry %s (%s)\n", res.AccessEntryARN, verb)
	}
	return 0
}

func runVerify(ctx context.Context) int {
	cfg, err := parseGlobalFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	mgr, err := registry.NewManager(ctx, cfg.ClusterName, cfg.ClusterRegion, cfg.Profile)
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
			i++
			cfg.ClusterName = argValue(args, i)
		case "--cluster-region":
			i++
			cfg.ClusterRegion = argValue(args, i)
		case "--profile":
			i++
			cfg.Profile = argValue(args, i)
		default:
			return nil, fmt.Errorf("unknown flag: %s", args[i])
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

// argValue safely returns args[i], or "" if i is out of range. Callers increment the
// index past a flag name before calling, so a missing trailing value yields "" rather
// than an index-out-of-range panic. The required-field checks then produce a clean error.
func argValue(args []string, i int) string {
	if i >= len(args) {
		return ""
	}
	return args[i]
}

func parseAddSatelliteFlags() (*addSatelliteConfig, error) {
	cfg := &addSatelliteConfig{}
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cluster-name":
			i++
			cfg.ClusterName = argValue(args, i)
		case "--cluster-region":
			i++
			cfg.ClusterRegion = argValue(args, i)
		case "--vpc-id":
			i++
			cfg.VPCID = argValue(args, i)
		case "--satellite-region":
			i++
			cfg.SatelliteRegion = argValue(args, i)
		case "--account-id":
			i++
			cfg.AccountID = argValue(args, i)
		case "--profile":
			i++
			cfg.Profile = argValue(args, i)
		case "--subnet-ids":
			i++
			cfg.SubnetIDs = splitComma(argValue(args, i))
		case "--security-group-ids":
			i++
			cfg.SecurityGroupIDs = splitComma(argValue(args, i))
		case "--with-eniconfigs":
			cfg.WithENIConfigs = true
		default:
			return nil, fmt.Errorf("unknown flag: %s", args[i])
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

func parseSetupIAMFlags() (*setupIAMConfig, error) {
	cfg := &setupIAMConfig{}
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cluster-name":
			i++
			cfg.ClusterName = argValue(args, i)
		case "--cluster-region":
			i++
			cfg.ClusterRegion = argValue(args, i)
		case "--profile":
			i++
			cfg.Profile = argValue(args, i)
		case "--node-role-name":
			i++
			cfg.NodeRoleName = argValue(args, i)
		case "--node-role-arn":
			i++
			cfg.NodeRoleARN = argValue(args, i)
		case "--node-role-only":
			cfg.NodeRoleOnly = true
		case "--access-entry-only":
			cfg.AccessEntryOnly = true
		default:
			return nil, fmt.Errorf("unknown flag: %s", args[i])
		}
	}
	if cfg.ClusterName == "" || cfg.ClusterRegion == "" {
		return nil, fmt.Errorf("--cluster-name and --cluster-region are required")
	}
	if cfg.NodeRoleOnly && cfg.AccessEntryOnly {
		return nil, fmt.Errorf("--node-role-only and --access-entry-only are mutually exclusive")
	}
	if !cfg.AccessEntryOnly && cfg.NodeRoleName == "" {
		return nil, fmt.Errorf("--node-role-name is required (unless --access-entry-only)")
	}
	if cfg.AccessEntryOnly && cfg.NodeRoleARN == "" && cfg.NodeRoleName == "" {
		return nil, fmt.Errorf("--access-entry-only requires --node-role-arn (the role lives in another account)")
	}
	return cfg, nil
}

func parseRemoveSatelliteFlags() (*removeSatelliteConfig, error) {
	cfg := &removeSatelliteConfig{}
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cluster-name":
			i++
			cfg.ClusterName = argValue(args, i)
		case "--cluster-region":
			i++
			cfg.ClusterRegion = argValue(args, i)
		case "--vpc-id":
			i++
			cfg.VPCID = argValue(args, i)
		case "--profile":
			i++
			cfg.Profile = argValue(args, i)
		default:
			return nil, fmt.Errorf("unknown flag: %s", args[i])
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
