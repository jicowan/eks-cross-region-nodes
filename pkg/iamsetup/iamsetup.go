// Package iamsetup creates the IAM prerequisites for cross-region / cross-account
// EKS satellite nodes: the node IAM role + instance profile (in the account where the
// nodes run) and the HYBRID_LINUX access entry (in the cluster account).
//
// These are split deliberately because they may live in different AWS accounts. Use the
// xrnctl --profile flag to select credentials for the relevant account:
//
//	# in the satellite account (creates role + instance profile)
//	xrnctl setup-iam --profile <satellite> --cluster-name ... --cluster-region ... --node-role-name CrossRegionNodeRole
//
//	# in the cluster account (creates the access entry for that role's ARN)
//	xrnctl setup-iam --profile <cluster> --cluster-name ... --cluster-region ... --node-role-arn arn:aws:iam::<acct>:role/CrossRegionNodeRole --access-entry-only
package iamsetup

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// managedPolicies are the AWS-managed policies every EKS worker node role needs.
// These are the same for cluster-VPC and satellite nodes; the only difference between
// a normal node role and a satellite node role is the access entry TYPE (HYBRID_LINUX),
// which is created separately.
var managedPolicies = []string{
	"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
	"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
	"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
	"arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
}

// iamAPI is the subset of the IAM client used here. Defined as an interface so tests can
// inject a fake; the concrete *iam.Client satisfies it.
type iamAPI interface {
	GetRole(ctx context.Context, in *iam.GetRoleInput, opts ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	CreateRole(ctx context.Context, in *iam.CreateRoleInput, opts ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	AttachRolePolicy(ctx context.Context, in *iam.AttachRolePolicyInput, opts ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error)
	GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, opts ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error)
	CreateInstanceProfile(ctx context.Context, in *iam.CreateInstanceProfileInput, opts ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error)
	AddRoleToInstanceProfile(ctx context.Context, in *iam.AddRoleToInstanceProfileInput, opts ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error)
}

// eksAPI is the subset of the EKS client used here.
type eksAPI interface {
	CreateAccessEntry(ctx context.Context, in *eks.CreateAccessEntryInput, opts ...func(*eks.Options)) (*eks.CreateAccessEntryOutput, error)
	DescribeAccessEntry(ctx context.Context, in *eks.DescribeAccessEntryInput, opts ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error)
}

// ec2AssumeRolePolicy lets EC2 instances assume the node role (instance profile use).
const ec2AssumeRolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {"Service": "ec2.amazonaws.com"},
      "Action": "sts:AssumeRole"
    }
  ]
}`

// Options configures a setup-iam run.
type Options struct {
	ClusterName   string
	ClusterRegion string
	Profile       string

	NodeRoleName string // IAM role name to create/reuse for satellite nodes
	NodeRoleARN  string // for --access-entry-only: the role ARN to attach the access entry to

	NodeRoleOnly    bool // create only the role + instance profile (satellite account)
	AccessEntryOnly bool // create only the access entry (cluster account)
}

// Result reports what was done.
type Result struct {
	RoleName            string
	RoleARN             string
	InstanceProfileName string
	InstanceProfileARN  string
	AccessEntryARN      string
	RoleCreated         bool
	InstanceProfileMade bool
	AccessEntryCreated  bool
}

// Run executes the requested IAM setup. By default it does both halves (role + instance
// profile, then access entry) assuming a single-account setup. For cross-account, run it
// twice with the appropriate --profile and the *Only flags.
func Run(ctx context.Context, opts Options) (*Result, error) {
	cfg, err := loadAWSConfig(ctx, opts.ClusterRegion, opts.Profile)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	res := &Result{}

	doRole := !opts.AccessEntryOnly
	doAccessEntry := !opts.NodeRoleOnly

	if doRole {
		if opts.NodeRoleName == "" {
			return nil, fmt.Errorf("--node-role-name is required to create the node role")
		}
		iamClient := iam.NewFromConfig(cfg)
		if err := ensureNodeRole(ctx, iamClient, opts.NodeRoleName, res); err != nil {
			return nil, err
		}
		if err := ensureInstanceProfile(ctx, iamClient, opts.NodeRoleName, res); err != nil {
			return nil, err
		}
	}

	if doAccessEntry {
		// The access entry needs the role ARN. If we just created the role, use that ARN;
		// otherwise the caller must supply --node-role-arn (cross-account: the role lives
		// in a different account than the cluster).
		roleARN := opts.NodeRoleARN
		if roleARN == "" {
			roleARN = res.RoleARN
		}
		if roleARN == "" {
			return nil, fmt.Errorf("access entry needs a role ARN: pass --node-role-arn, or create the role in the same run")
		}
		eksClient := eks.NewFromConfig(cfg)
		if err := ensureAccessEntry(ctx, eksClient, opts.ClusterName, roleARN, res); err != nil {
			return nil, err
		}
	}

	return res, nil
}

func ensureNodeRole(ctx context.Context, c iamAPI, name string, res *Result) error {
	out, err := c.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err == nil {
		res.RoleName = name
		res.RoleARN = aws.ToString(out.Role.Arn)
	} else if isNotFound(err) {
		created, cerr := c.CreateRole(ctx, &iam.CreateRoleInput{
			RoleName:                 aws.String(name),
			AssumeRolePolicyDocument: aws.String(ec2AssumeRolePolicy),
			Description:              aws.String("EKS cross-region/cross-account satellite node role (managed by xrnctl)"),
		})
		if cerr != nil {
			return fmt.Errorf("iam:CreateRole %s: %w", name, cerr)
		}
		res.RoleName = name
		res.RoleARN = aws.ToString(created.Role.Arn)
		res.RoleCreated = true
	} else {
		return fmt.Errorf("iam:GetRole %s: %w", name, err)
	}

	// Attach managed policies (idempotent — attaching an already-attached policy is a no-op).
	for _, p := range managedPolicies {
		_, aerr := c.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
			RoleName:  aws.String(name),
			PolicyArn: aws.String(p),
		})
		if aerr != nil {
			return fmt.Errorf("iam:AttachRolePolicy %s -> %s: %w", name, p, aerr)
		}
	}
	return nil
}

func ensureInstanceProfile(ctx context.Context, c iamAPI, roleName string, res *Result) error {
	// Convention: instance profile name == role name. Simplest for operators to reason about.
	profileName := roleName

	_, err := c.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	})
	if isNotFound(err) {
		created, cerr := c.CreateInstanceProfile(ctx, &iam.CreateInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
		})
		if cerr != nil {
			return fmt.Errorf("iam:CreateInstanceProfile %s: %w", profileName, cerr)
		}
		res.InstanceProfileARN = aws.ToString(created.InstanceProfile.Arn)
		res.InstanceProfileMade = true
	} else if err != nil {
		return fmt.Errorf("iam:GetInstanceProfile %s: %w", profileName, err)
	}
	res.InstanceProfileName = profileName

	// Ensure the role is in the instance profile. AddRoleToInstanceProfile errors if a
	// (different) role is already attached, so check first.
	gp, gerr := c.GetInstanceProfile(ctx, &iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
	})
	if gerr != nil {
		return fmt.Errorf("iam:GetInstanceProfile %s (post-create): %w", profileName, gerr)
	}
	if res.InstanceProfileARN == "" {
		res.InstanceProfileARN = aws.ToString(gp.InstanceProfile.Arn)
	}
	for _, r := range gp.InstanceProfile.Roles {
		if aws.ToString(r.RoleName) == roleName {
			return nil // already attached
		}
	}
	if len(gp.InstanceProfile.Roles) > 0 {
		return fmt.Errorf("instance profile %s already has a different role (%s); refusing to change it",
			profileName, aws.ToString(gp.InstanceProfile.Roles[0].RoleName))
	}
	_, aerr := c.AddRoleToInstanceProfile(ctx, &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String(profileName),
		RoleName:            aws.String(roleName),
	})
	if aerr != nil {
		return fmt.Errorf("iam:AddRoleToInstanceProfile %s -> %s: %w", roleName, profileName, aerr)
	}
	return nil
}

func ensureAccessEntry(ctx context.Context, c eksAPI, clusterName, roleARN string, res *Result) error {
	_, err := c.CreateAccessEntry(ctx, &eks.CreateAccessEntryInput{
		ClusterName:  aws.String(clusterName),
		PrincipalArn: aws.String(roleARN),
		Type:         aws.String("HYBRID_LINUX"),
	})
	if err != nil {
		// Already exists is fine — verify it's the right type.
		var rie *ekstypes.ResourceInUseException
		if errors.As(err, &rie) {
			detail, derr := c.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{
				ClusterName:  aws.String(clusterName),
				PrincipalArn: aws.String(roleARN),
			})
			if derr != nil {
				return fmt.Errorf("access entry exists but DescribeAccessEntry failed: %w", derr)
			}
			if t := aws.ToString(detail.AccessEntry.Type); t != "HYBRID_LINUX" {
				return fmt.Errorf("access entry for %s already exists with type %s (need HYBRID_LINUX); delete it and re-run", roleARN, t)
			}
			res.AccessEntryARN = aws.ToString(detail.AccessEntry.AccessEntryArn)
			return nil
		}
		return fmt.Errorf("eks:CreateAccessEntry for %s: %w", roleARN, err)
	}
	res.AccessEntryCreated = true
	// Fetch the ARN for reporting.
	detail, derr := c.DescribeAccessEntry(ctx, &eks.DescribeAccessEntryInput{
		ClusterName:  aws.String(clusterName),
		PrincipalArn: aws.String(roleARN),
	})
	if derr == nil {
		res.AccessEntryARN = aws.ToString(detail.AccessEntry.AccessEntryArn)
	}
	return nil
}

func loadAWSConfig(ctx context.Context, region, profile string) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}

// isNotFound reports whether err is an IAM NoSuchEntity error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nse *iamtypes.NoSuchEntityException
	return errors.As(err, &nse)
}
