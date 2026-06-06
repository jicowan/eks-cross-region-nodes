package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/eks"
)

type ClusterInfo struct {
	Name            string   `json:"name"`
	Region          string   `json:"region"`
	Endpoint        string   `json:"endpoint"`
	CertificateAuth string   `json:"certificateAuthority"`
	ServiceCIDR     string   `json:"serviceCidr"`
	VPCID           string   `json:"vpcId"`
	VPCCIDRs        []string `json:"vpcCidrs"`
	SecurityGroupID string   `json:"clusterSecurityGroupId"`
	ARN             string   `json:"arn"`
	AccountID       string   `json:"accountId"` // parsed from the cluster ARN
}

type NodeMetadata struct {
	InstanceID       string `json:"instanceId"`
	Region           string `json:"region"`
	AvailabilityZone string `json:"availabilityZone"`
	VPCID            string `json:"vpcId"`
	SubnetID         string `json:"subnetId"`
	PrivateIP        string `json:"privateIp"`
	InstanceType     string `json:"instanceType"`
	AccountID        string `json:"accountId"`
}

func DescribeCluster(ctx context.Context, clusterName, clusterRegion string) (*ClusterInfo, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(clusterRegion))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config for region %s: %w", clusterRegion, err)
	}

	client := eks.NewFromConfig(cfg)
	out, err := client.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(clusterName),
	})
	if err != nil {
		return nil, fmt.Errorf("eks:DescribeCluster: %w", err)
	}

	cluster := out.Cluster
	info := &ClusterInfo{
		Name:            clusterName,
		Region:          clusterRegion,
		Endpoint:        aws.ToString(cluster.Endpoint),
		CertificateAuth: aws.ToString(cluster.CertificateAuthority.Data),
		SecurityGroupID: aws.ToString(cluster.ResourcesVpcConfig.ClusterSecurityGroupId),
		VPCID:           aws.ToString(cluster.ResourcesVpcConfig.VpcId),
		ARN:             aws.ToString(cluster.Arn),
	}
	info.AccountID = AccountIDFromARN(info.ARN)

	if cluster.KubernetesNetworkConfig != nil && cluster.KubernetesNetworkConfig.ServiceIpv4Cidr != nil {
		info.ServiceCIDR = aws.ToString(cluster.KubernetesNetworkConfig.ServiceIpv4Cidr)
	}

	return info, nil
}

// AccountIDFromARN extracts the account field (index 4) from an ARN, or "" if it can't be
// parsed. arn:partition:service:region:account-id:resource.
func AccountIDFromARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 {
		return ""
	}
	return parts[4]
}

func GetNodeMetadata(ctx context.Context) (*NodeMetadata, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := imds.NewFromConfig(cfg)

	instanceID, err := getIMDS(ctx, client, "instance-id")
	if err != nil {
		return nil, fmt.Errorf("getting instance-id from IMDS: %w", err)
	}

	region, err := getIMDS(ctx, client, "placement/region")
	if err != nil {
		return nil, fmt.Errorf("getting region from IMDS: %w", err)
	}

	az, err := getIMDS(ctx, client, "placement/availability-zone")
	if err != nil {
		return nil, fmt.Errorf("getting AZ from IMDS: %w", err)
	}

	mac, err := getIMDS(ctx, client, "mac")
	if err != nil {
		return nil, fmt.Errorf("getting MAC from IMDS: %w", err)
	}

	vpcID, err := getIMDS(ctx, client, fmt.Sprintf("network/interfaces/macs/%s/vpc-id", mac))
	if err != nil {
		return nil, fmt.Errorf("getting VPC ID from IMDS: %w", err)
	}

	subnetID, err := getIMDS(ctx, client, fmt.Sprintf("network/interfaces/macs/%s/subnet-id", mac))
	if err != nil {
		return nil, fmt.Errorf("getting subnet ID from IMDS: %w", err)
	}

	privateIP, err := getIMDS(ctx, client, "local-ipv4")
	if err != nil {
		return nil, fmt.Errorf("getting private IP from IMDS: %w", err)
	}

	instanceType, err := getIMDS(ctx, client, "instance-type")
	if err != nil {
		return nil, fmt.Errorf("getting instance type from IMDS: %w", err)
	}

	// Get account ID from identity document
	idDoc, err := client.GetInstanceIdentityDocument(ctx, &imds.GetInstanceIdentityDocumentInput{})
	if err != nil {
		return nil, fmt.Errorf("getting identity document: %w", err)
	}

	return &NodeMetadata{
		InstanceID:       instanceID,
		Region:           region,
		AvailabilityZone: az,
		VPCID:            vpcID,
		SubnetID:         subnetID,
		PrivateIP:        privateIP,
		InstanceType:     instanceType,
		AccountID:        idDoc.AccountID,
	}, nil
}

func getIMDS(ctx context.Context, client *imds.Client, path string) (string, error) {
	out, err := client.GetMetadata(ctx, &imds.GetMetadataInput{Path: path})
	if err != nil {
		return "", err
	}
	defer out.Content.Close()
	buf := make([]byte, 1024)
	n, _ := out.Content.Read(buf)
	return string(buf[:n]), nil
}

func PrintJSON(cluster *ClusterInfo, node *NodeMetadata) {
	out := struct {
		Cluster *ClusterInfo  `json:"cluster"`
		Node    *NodeMetadata `json:"node"`
	}{cluster, node}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(out)
}
