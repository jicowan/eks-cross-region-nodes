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

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	ctx := context.Background()

	switch os.Args[1] {
	case "init":
		os.Exit(runInit(ctx))
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
  xrn-install init       --cluster-name NAME --cluster-region REGION
  xrn-install preflight  --cluster-name NAME --cluster-region REGION
  xrn-install discover   --cluster-name NAME --cluster-region REGION
  xrn-install version

Subcommands:
  init        Bootstrap this node into a cross-region EKS cluster (discovery + preflight + nodeadm + patch)
  preflight   Run pre-flight checks only, exit 0 if all pass
  discover    Print discovered cluster config as JSON without applying
  version     Print version
`)
}

func runInit(ctx context.Context) int {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Step 1: Discover cluster configuration
	fmt.Println("[1/4] Discovering cluster configuration...")
	cluster, err := discovery.DescribeCluster(ctx, cfg.ClusterName, cfg.ClusterRegion)
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

	fmt.Println("  Patching kubelet configuration for cross-region...")
	if err := patch.ApplyAll(ctx, cluster, node); err != nil {
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
	ClusterName   string
	ClusterRegion string
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
