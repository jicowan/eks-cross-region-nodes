package iamsetup

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

// fakeIAM implements iamAPI with programmable behavior and call tracking.
type fakeIAM struct {
	roleExists    bool
	roleARN       string
	profileExists bool
	profileARN    string
	profileRoles  []string // role names already attached to the instance profile

	createdRole        bool
	createdProfile     bool
	attachedPolicies   []string
	addedRoleToProfile bool
}

func (f *fakeIAM) GetRole(ctx context.Context, in *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	if f.roleExists {
		return &iam.GetRoleOutput{Role: &iamtypes.Role{Arn: aws.String(f.roleARN)}}, nil
	}
	return nil, &iamtypes.NoSuchEntityException{}
}

func (f *fakeIAM) CreateRole(ctx context.Context, in *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.createdRole = true
	f.roleExists = true
	if f.roleARN == "" {
		f.roleARN = "arn:aws:iam::111122223333:role/" + aws.ToString(in.RoleName)
	}
	return &iam.CreateRoleOutput{Role: &iamtypes.Role{Arn: aws.String(f.roleARN)}}, nil
}

func (f *fakeIAM) AttachRolePolicy(ctx context.Context, in *iam.AttachRolePolicyInput, _ ...func(*iam.Options)) (*iam.AttachRolePolicyOutput, error) {
	f.attachedPolicies = append(f.attachedPolicies, aws.ToString(in.PolicyArn))
	return &iam.AttachRolePolicyOutput{}, nil
}

func (f *fakeIAM) GetInstanceProfile(ctx context.Context, in *iam.GetInstanceProfileInput, _ ...func(*iam.Options)) (*iam.GetInstanceProfileOutput, error) {
	if !f.profileExists {
		return nil, &iamtypes.NoSuchEntityException{}
	}
	roles := make([]iamtypes.Role, 0, len(f.profileRoles))
	for _, rn := range f.profileRoles {
		roles = append(roles, iamtypes.Role{RoleName: aws.String(rn)})
	}
	arn := f.profileARN
	if arn == "" {
		arn = "arn:aws:iam::111122223333:instance-profile/" + aws.ToString(in.InstanceProfileName)
	}
	return &iam.GetInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{
		Arn:   aws.String(arn),
		Roles: roles,
	}}, nil
}

func (f *fakeIAM) CreateInstanceProfile(ctx context.Context, in *iam.CreateInstanceProfileInput, _ ...func(*iam.Options)) (*iam.CreateInstanceProfileOutput, error) {
	f.createdProfile = true
	f.profileExists = true
	if f.profileARN == "" {
		f.profileARN = "arn:aws:iam::111122223333:instance-profile/" + aws.ToString(in.InstanceProfileName)
	}
	return &iam.CreateInstanceProfileOutput{InstanceProfile: &iamtypes.InstanceProfile{Arn: aws.String(f.profileARN)}}, nil
}

func (f *fakeIAM) AddRoleToInstanceProfile(ctx context.Context, in *iam.AddRoleToInstanceProfileInput, _ ...func(*iam.Options)) (*iam.AddRoleToInstanceProfileOutput, error) {
	f.addedRoleToProfile = true
	f.profileRoles = append(f.profileRoles, aws.ToString(in.RoleName))
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

// fakeEKS implements eksAPI.
type fakeEKS struct {
	entryExists bool
	entryType   string
}

func (f *fakeEKS) CreateAccessEntry(ctx context.Context, in *eks.CreateAccessEntryInput, _ ...func(*eks.Options)) (*eks.CreateAccessEntryOutput, error) {
	if f.entryExists {
		return nil, &ekstypes.ResourceInUseException{}
	}
	f.entryExists = true
	f.entryType = aws.ToString(in.Type)
	return &eks.CreateAccessEntryOutput{}, nil
}

func (f *fakeEKS) DescribeAccessEntry(ctx context.Context, in *eks.DescribeAccessEntryInput, _ ...func(*eks.Options)) (*eks.DescribeAccessEntryOutput, error) {
	return &eks.DescribeAccessEntryOutput{AccessEntry: &ekstypes.AccessEntry{
		AccessEntryArn: aws.String("arn:aws:eks:us-east-2:111122223333:access-entry/main/role/111122223333/CrossRegionNodeRole/abc"),
		Type:           aws.String(f.entryType),
	}}, nil
}

func TestEnsureNodeRole_CreatesWhenMissing(t *testing.T) {
	f := &fakeIAM{roleExists: false}
	res := &Result{}
	if err := ensureNodeRole(context.Background(), f, "CrossRegionNodeRole", res); err != nil {
		t.Fatalf("ensureNodeRole: %v", err)
	}
	if !f.createdRole {
		t.Error("expected role to be created")
	}
	if !res.RoleCreated {
		t.Error("Result.RoleCreated should be true")
	}
	if len(f.attachedPolicies) != len(managedPolicies) {
		t.Errorf("attached %d policies, want %d", len(f.attachedPolicies), len(managedPolicies))
	}
}

func TestEnsureNodeRole_ReusesExisting(t *testing.T) {
	f := &fakeIAM{roleExists: true, roleARN: "arn:aws:iam::111122223333:role/CrossRegionNodeRole"}
	res := &Result{}
	if err := ensureNodeRole(context.Background(), f, "CrossRegionNodeRole", res); err != nil {
		t.Fatalf("ensureNodeRole: %v", err)
	}
	if f.createdRole {
		t.Error("should not create a role that already exists")
	}
	if res.RoleCreated {
		t.Error("Result.RoleCreated should be false for existing role")
	}
	// Policies still attached (idempotent).
	if len(f.attachedPolicies) != len(managedPolicies) {
		t.Errorf("attached %d policies, want %d", len(f.attachedPolicies), len(managedPolicies))
	}
	if res.RoleARN != "arn:aws:iam::111122223333:role/CrossRegionNodeRole" {
		t.Errorf("RoleARN = %q", res.RoleARN)
	}
}

func TestEnsureInstanceProfile_CreatesAndAttaches(t *testing.T) {
	f := &fakeIAM{profileExists: false}
	res := &Result{}
	if err := ensureInstanceProfile(context.Background(), f, "CrossRegionNodeRole", res); err != nil {
		t.Fatalf("ensureInstanceProfile: %v", err)
	}
	if !f.createdProfile {
		t.Error("expected instance profile to be created")
	}
	if !f.addedRoleToProfile {
		t.Error("expected role to be added to the instance profile")
	}
	if !res.InstanceProfileMade {
		t.Error("Result.InstanceProfileMade should be true")
	}
}

func TestEnsureInstanceProfile_AlreadyHasCorrectRole(t *testing.T) {
	f := &fakeIAM{profileExists: true, profileRoles: []string{"CrossRegionNodeRole"}}
	res := &Result{}
	if err := ensureInstanceProfile(context.Background(), f, "CrossRegionNodeRole", res); err != nil {
		t.Fatalf("ensureInstanceProfile: %v", err)
	}
	if f.createdProfile {
		t.Error("should not create existing profile")
	}
	if f.addedRoleToProfile {
		t.Error("should not re-add a role that is already attached")
	}
}

func TestEnsureInstanceProfile_RejectsDifferentRole(t *testing.T) {
	f := &fakeIAM{profileExists: true, profileRoles: []string{"SomeOtherRole"}}
	res := &Result{}
	err := ensureInstanceProfile(context.Background(), f, "CrossRegionNodeRole", res)
	if err == nil {
		t.Fatal("expected error when profile already has a different role")
	}
}

func TestEnsureAccessEntry_CreatesWhenMissing(t *testing.T) {
	f := &fakeEKS{entryExists: false}
	res := &Result{}
	if err := ensureAccessEntry(context.Background(), f, "main", "arn:aws:iam::111122223333:role/CrossRegionNodeRole", res); err != nil {
		t.Fatalf("ensureAccessEntry: %v", err)
	}
	if !res.AccessEntryCreated {
		t.Error("Result.AccessEntryCreated should be true")
	}
	if f.entryType != "HYBRID_LINUX" {
		t.Errorf("created entry type = %q, want HYBRID_LINUX", f.entryType)
	}
}

func TestEnsureAccessEntry_ExistingCorrectType(t *testing.T) {
	f := &fakeEKS{entryExists: true, entryType: "HYBRID_LINUX"}
	res := &Result{}
	if err := ensureAccessEntry(context.Background(), f, "main", "arn:aws:iam::111122223333:role/CrossRegionNodeRole", res); err != nil {
		t.Fatalf("ensureAccessEntry: %v", err)
	}
	if res.AccessEntryCreated {
		t.Error("should not report created for a pre-existing entry")
	}
	if res.AccessEntryARN == "" {
		t.Error("should still report the existing entry ARN")
	}
}

func TestEnsureAccessEntry_ExistingWrongType(t *testing.T) {
	f := &fakeEKS{entryExists: true, entryType: "EC2_LINUX"}
	res := &Result{}
	err := ensureAccessEntry(context.Background(), f, "main", "arn:aws:iam::111122223333:role/CrossRegionNodeRole", res)
	if err == nil {
		t.Fatal("expected error when existing access entry has the wrong type")
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(&iamtypes.NoSuchEntityException{}) {
		t.Error("NoSuchEntityException should be recognized as not-found")
	}
	if isNotFound(nil) {
		t.Error("nil should not be not-found")
	}
	if isNotFound(&ekstypes.ResourceInUseException{}) {
		t.Error("ResourceInUseException should not be not-found")
	}
}
