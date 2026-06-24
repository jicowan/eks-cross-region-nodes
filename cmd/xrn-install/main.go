package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/eks-cross-region-nodes/pkg/bootstrap"
	"github.com/aws/eks-cross-region-nodes/pkg/discovery"
	"github.com/aws/eks-cross-region-nodes/pkg/patch"
	"github.com/aws/eks-cross-region-nodes/pkg/preflight"
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
	case "init":
		os.Exit(runInit(ctx))
	case "patch":
		os.Exit(runPatch(ctx))
	case "preflight":
		os.Exit(runPreflight(ctx))
	case "discover":
		os.Exit(runDiscover(ctx))
	case "version":
		fmt.Printf("xrn-install %s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `xrn-install — Cross-region EKS node installer

Usage:
  xrn-install init       --cluster-name NAME --cluster-region REGION [--cluster-account-role-arn ARN] [--cluster-account-external-id ID] [--provider-id-format eks-hybrid|aws]
  xrn-install patch      --cluster-name NAME --cluster-region REGION [--cluster-account-role-arn ARN] [--cluster-account-external-id ID] [--provider-id-format eks-hybrid|aws]
  xrn-install preflight  --cluster-name NAME --cluster-region REGION
  xrn-install discover   --cluster-name NAME --cluster-region REGION
  xrn-install version

Subcommands:
  init        Bootstrap this node into a cross-region EKS cluster (discovery + preflight + nodeadm + patch + restart)
  patch       Apply the kubelet patches only (no nodeadm, no restart). For the pre-kubelet
              boothook flow: run from a kubelet.service ExecStartPre so the first kubelet
              start already has the correct config. Required for cross-account nodes.
  preflight   Run pre-flight checks only, exit 0 if all pass
  discover    Print discovered cluster config as JSON without applying
  version     Print version

Cross-account:
  When the node's account differs from the cluster's account, pass
  --cluster-account-role-arn — the role (in the cluster account) the instance assumes for
  kubelet credentials (e.g. arn:aws:iam::<cluster-acct>:role/XrnSatelliteNodeRole). The
  installer auto-detects the account mismatch and errors with a hint if the flag is missing.
  --cluster-account-external-id is optional (sts:ExternalId on the AssumeRole).

providerID format:
  --provider-id-format aws (default) writes providerID=aws:///<az>/<id>, the standard EC2 form. It is
  CCM-safe (with --cloud-provider="" the CCM does not reap the node — validated same- and
  cross-account) AND parseable by the cluster-autoscaler AWS provider, so CA can manage these nodes.
  --provider-id-format eks-hybrid writes eks-hybrid:///<region>/<cluster>/<id> (legacy escape hatch);
  also CCM-safe but NOT parseable by cluster-autoscaler, which will delete the node as
  longUnregistered.
`)
}

func runInit(ctx context.Context) int {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Step 1: Discover cluster configuration. On cross-account nodes the instance role can't
	// see the cluster, so DescribeClusterWithRole assumes the cluster-account role first when
	// --cluster-account-role-arn is set.
	fmt.Println("[1/4] Discovering cluster configuration...")
	cluster, err := discovery.DescribeClusterWithRole(ctx, cfg.ClusterName, cfg.ClusterRegion, cfg.ClusterAccountRoleARN, cfg.ClusterAccountExtID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to discover cluster: %v\n", err)
		return 12
	}

	// Step 2: Discover node metadata
	fmt.Println("[2/4] Discovering node metadata...")
	node, err := discovery.GetNodeMetadata(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to get node metadata: %v\n", err)
		return 10
	}

	// Step 3: Pre-flight checks
	fmt.Println("[3/4] Running pre-flight checks...")
	results := preflight.RunAll(ctx, cluster, node)

	// Print warnings (advisory, don't block install)
	for _, r := range results.Warnings() {
		fmt.Fprintf(os.Stderr, "  ⚠ WARNING: %s\n    %s\n", r.Name, r.Error)
	}

	if !results.AllPassed() {
		fmt.Fprintf(os.Stderr, "\nPre-flight checks failed:\n")
		for _, r := range results.Failed() {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %s\n", r.Name, r.Error)
		}
		return results.FirstFailedExitCode()
	}
	fmt.Println("  All blocking pre-flight checks passed.")

	// Step 4: Bootstrap (if needed) and patch
	fmt.Println("[4/4] Bootstrapping node...")
	skipped, err := bootstrap.RunNodeadmIfNeeded(ctx, cluster, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: nodeadm failed: %v\n", err)
		return 1
	}
	if skipped {
		fmt.Println("  nodeadm already ran (kubeconfig exists), skipping bootstrap.")
	}

	xacct, err := resolveCrossAccount(cfg, cluster, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 14
	}
	if xacct != nil {
		fmt.Printf("  Cross-account detected: assuming %s for kubelet credentials.\n", xacct.SatelliteRoleARN)
		fmt.Println("  Patching kubelet configuration for cross-region (with AssumeRole credential helper)...")
	} else {
		fmt.Println("  Patching kubelet configuration for cross-region...")
	}
	if err := patch.ApplyAll(ctx, cluster, node, xacct, cfg.ProviderIDFormat); err != nil {
		fmt.Fprintf(os.Stderr, "error: patch failed: %v\n", err)
		return 1
	}

	fmt.Println("  Restarting kubelet...")
	if err := bootstrap.RestartKubelet(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: kubelet restart failed: %v\n", err)
		return 1
	}

	fmt.Printf("\n✓ Node %s successfully bootstrapped into cluster %s (region %s)\n", node.InstanceID, cfg.ClusterName, cfg.ClusterRegion)
	return 0
}

// runPatch applies the cross-region kubelet patches WITHOUT running nodeadm or restarting
// kubelet. It is meant to be invoked from a kubelet.service ExecStartPre drop-in (the
// pre-kubelet boothook flow): nodeadm-config has already written the config files, kubelet
// is starting and blocks on this, so the very first kubelet start uses the patched config.
// This is the cross-account path — the providerID must be correct before kubelet ever
// registers, since cross-account nodes don't get CCM's grace.
func runPatch(ctx context.Context) int {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	cluster, err := discovery.DescribeClusterWithRole(ctx, cfg.ClusterName, cfg.ClusterRegion, cfg.ClusterAccountRoleARN, cfg.ClusterAccountExtID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to discover cluster: %v\n", err)
		return 12
	}
	node, err := discovery.GetNodeMetadata(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to get node metadata: %v\n", err)
		return 10
	}

	xacct, err := resolveCrossAccount(cfg, cluster, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 14
	}

	if err := patch.ApplyAll(ctx, cluster, node, xacct, cfg.ProviderIDFormat); err != nil {
		fmt.Fprintf(os.Stderr, "error: patch failed: %v\n", err)
		return 1
	}
	fmt.Printf("xrn-install: patched kubelet config for node %s (cluster %s/%s)\n", node.InstanceID, cfg.ClusterRegion, cfg.ClusterName)
	return 0
}

func runPreflight(ctx context.Context) int {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	cluster, err := discovery.DescribeCluster(ctx, cfg.ClusterName, cfg.ClusterRegion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 12
	}

	node, err := discovery.GetNodeMetadata(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 10
	}

	results := preflight.RunAll(ctx, cluster, node)
	for _, r := range results.All() {
		switch {
		case r.Passed:
			fmt.Printf("  ✓ %s\n", r.Name)
		case r.Warning:
			fmt.Printf("  ⚠ %s: %s\n", r.Name, r.Error)
		default:
			fmt.Printf("  ✗ %s: %s\n", r.Name, r.Error)
		}
	}

	if results.AllPassed() {
		if len(results.Warnings()) > 0 {
			fmt.Println("\nAll blocking checks passed (with warnings).")
		} else {
			fmt.Println("\nAll checks passed.")
		}
		return 0
	}
	return results.FirstFailedExitCode()
}

func runDiscover(ctx context.Context) int {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	cluster, err := discovery.DescribeCluster(ctx, cfg.ClusterName, cfg.ClusterRegion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 12
	}

	node, err := discovery.GetNodeMetadata(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 10
	}

	discovery.PrintJSON(cluster, node)
	return 0
}

type config struct {
	ClusterName           string
	ClusterRegion         string
	ClusterAccountRoleARN string // cross-account: role in the cluster account the instance assumes
	ClusterAccountExtID   string // optional sts:ExternalId for the AssumeRole
	ProviderIDFormat      string // "eks-hybrid" (default) or "aws"; see pkg/patch ProviderID* consts
}

func parseFlags() (*config, error) {
	cfg := &config{}
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
		case "--cluster-account-role-arn":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--cluster-account-role-arn requires a value")
			}
			i++
			cfg.ClusterAccountRoleARN = args[i]
		case "--cluster-account-external-id":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--cluster-account-external-id requires a value")
			}
			i++
			cfg.ClusterAccountExtID = args[i]
		case "--provider-id-format":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("--provider-id-format requires a value")
			}
			i++
			cfg.ProviderIDFormat = args[i]
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
	switch cfg.ProviderIDFormat {
	case "", patch.ProviderIDHybrid, patch.ProviderIDAWS:
		// ok
	default:
		return nil, fmt.Errorf("--provider-id-format must be %q or %q, got %q", patch.ProviderIDHybrid, patch.ProviderIDAWS, cfg.ProviderIDFormat)
	}
	return cfg, nil
}

// resolveCrossAccount decides whether this install is cross-account by comparing the
// instance's account (from IMDS) to the cluster's account (from the cluster ARN), and
// validates the flag. Returns nil for the same-account case.
//
// Rules:
//   - accounts match           → same-account, return nil (no flag needed)
//   - accounts differ + flag   → cross-account, return the CrossAccount config
//   - accounts differ + NO flag → error with a copy-pasteable hint
//   - cluster account unknown   → fall back to flag presence (explicit opt-in)
func resolveCrossAccount(cfg *config, cluster *discovery.ClusterInfo, node *discovery.NodeMetadata) (*patch.CrossAccount, error) {
	clusterAcct := cluster.AccountID
	instanceAcct := node.AccountID

	mismatch := clusterAcct != "" && instanceAcct != "" && clusterAcct != instanceAcct

	if cfg.ClusterAccountRoleARN == "" {
		if mismatch {
			return nil, fmt.Errorf(
				"instance is in account %s but cluster is in account %s; set --cluster-account-role-arn arn:aws:iam::%s:role/XrnSatelliteNodeRole",
				instanceAcct, clusterAcct, clusterAcct)
		}
		return nil, nil // same-account
	}

	// Flag is set. Use it (covers the mismatch case and the unknown-cluster-account case).
	return &patch.CrossAccount{
		Enabled:          true,
		SatelliteRoleARN: cfg.ClusterAccountRoleARN,
		ExternalID:       cfg.ClusterAccountExtID,
	}, nil
}
