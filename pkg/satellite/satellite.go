// Package satellite renders the Kubernetes manifests for a cross-account satellite's
// dedicated aws-node DaemonSet.
//
// A cross-account satellite node cannot use the cluster's stock aws-node DaemonSet: that
// DS's ServiceAccount has an EKS Pod Identity association that hands back cluster-account
// credentials, which are useless for managing ENIs in the satellite account. The fix
// (validated 2026-06-05) is a parallel DaemonSet whose ServiceAccount has NO Pod Identity
// association, so the AWS SDK falls through to IMDS and uses the satellite-account instance
// role. See docs/PRD-cross-account-nodes.md §5.4.
//
// One DS is rendered per (account, region) pair, named aws-node-satellite-<account>-<region>,
// and scoped via nodeAffinity to nodes carrying:
//   - eks.amazonaws.com/compute-type=hybrid
//   - xrn.amazonaws.com/satellite-account=<account>
//   - topology.kubernetes.io/region=<region>
package satellite

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// Default image coordinates. The VPC CNI add-on images live in a regional ECR account;
// 602401143452 is the public EKS add-on registry. Callers can override Registry/Region
// and the version tags to match their cluster's installed add-on versions.
const (
	DefaultRegistry          = "602401143452.dkr.ecr.us-east-2.amazonaws.com"
	DefaultCNIImageTag       = "v1.21.1-eksbuild.8"
	DefaultInitImageTag      = "v1.21.1-eksbuild.8"
	DefaultNodeAgentImageTag = "v1.3.4-eksbuild.1"
	DefaultCNIVersion        = "v1.21.1"
)

// Params configures manifest rendering for one (account, region) satellite.
type Params struct {
	AccountID        string   // satellite AWS account ID (different from cluster account)
	Region           string   // satellite region, used for AWS_REGION
	ClusterName      string   // EKS cluster name (CLUSTER_NAME env)
	ExcludeSNATCIDRs []string // cluster + all satellite VPC CIDRs

	// Image coordinates. If Registry is empty the defaults above are used.
	Registry          string
	CNIImageTag       string
	InitImageTag      string
	NodeAgentImageTag string
	CNIVersion        string
}

// Name returns the DaemonSet/ServiceAccount/ClusterRoleBinding name for these params.
func (p Params) Name() string {
	return fmt.Sprintf("aws-node-satellite-%s-%s", p.AccountID, p.Region)
}

func (p Params) withDefaults() Params {
	if p.Registry == "" {
		p.Registry = DefaultRegistry
	}
	if p.CNIImageTag == "" {
		p.CNIImageTag = DefaultCNIImageTag
	}
	if p.InitImageTag == "" {
		p.InitImageTag = DefaultInitImageTag
	}
	if p.NodeAgentImageTag == "" {
		p.NodeAgentImageTag = DefaultNodeAgentImageTag
	}
	if p.CNIVersion == "" {
		p.CNIVersion = DefaultCNIVersion
	}
	return p
}

// Validate checks required fields.
func (p Params) Validate() error {
	if p.AccountID == "" {
		return fmt.Errorf("AccountID is required")
	}
	if p.Region == "" {
		return fmt.Errorf("Region is required")
	}
	if p.ClusterName == "" {
		return fmt.Errorf("ClusterName is required")
	}
	return nil
}

type templateData struct {
	Params
	Name           string
	ExcludeSNATCSV string
	CNIImage       string
	InitImage      string
	NodeAgentImage string
}

// Render returns the multi-document YAML (ServiceAccount, ClusterRoleBinding, DaemonSet)
// for the satellite. The ClusterRoleBinding reuses the existing cluster-installed
// "aws-node" ClusterRole, so no Role needs to be created.
func Render(p Params) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	p = p.withDefaults()

	data := templateData{
		Params:         p,
		Name:           p.Name(),
		ExcludeSNATCSV: strings.Join(p.ExcludeSNATCIDRs, ","),
		CNIImage:       fmt.Sprintf("%s/amazon-k8s-cni:%s", p.Registry, p.CNIImageTag),
		InitImage:      fmt.Sprintf("%s/amazon-k8s-cni-init:%s", p.Registry, p.InitImageTag),
		NodeAgentImage: fmt.Sprintf("%s/amazon/aws-network-policy-agent:%s", p.Registry, p.NodeAgentImageTag),
	}

	tmpl, err := template.New("satellite").Parse(manifestTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering template: %w", err)
	}
	return buf.String(), nil
}

const manifestTemplate = `---
# ServiceAccount for the satellite VPC CNI. Intentionally has NO Pod Identity association,
# so the AWS SDK falls through to IMDS and uses the satellite-account instance role
# ({{.AccountID}}) for ec2:CreateNetworkInterface, etc.
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{.Name}}
  namespace: kube-system
  labels:
    app.kubernetes.io/name: {{.Name}}
    k8s-app: {{.Name}}
    xrn.amazonaws.com/satellite-account: "{{.AccountID}}"
---
# Reuse the cluster's existing aws-node ClusterRole; only the SA binding is new.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{.Name}}
  labels:
    k8s-app: {{.Name}}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: aws-node
subjects:
- kind: ServiceAccount
  name: {{.Name}}
  namespace: kube-system
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: {{.Name}}
  namespace: kube-system
  labels:
    k8s-app: {{.Name}}
spec:
  selector:
    matchLabels:
      k8s-app: {{.Name}}
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 10%
  template:
    metadata:
      labels:
        k8s-app: {{.Name}}
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: kubernetes.io/os
                operator: In
                values: ["linux"]
              - key: kubernetes.io/arch
                operator: In
                values: ["amd64", "arm64"]
              - key: eks.amazonaws.com/compute-type
                operator: In
                values: ["hybrid"]
              - key: xrn.amazonaws.com/satellite-account
                operator: In
                values: ["{{.AccountID}}"]
              - key: topology.kubernetes.io/region
                operator: In
                values: ["{{.Region}}"]
      hostNetwork: true
      priorityClassName: system-node-critical
      serviceAccountName: {{.Name}}
      terminationGracePeriodSeconds: 10
      tolerations:
      - operator: Exists
      initContainers:
      - name: aws-vpc-cni-init
        image: {{.InitImage}}
        imagePullPolicy: IfNotPresent
        env:
        - name: DISABLE_TCP_EARLY_DEMUX
          value: "false"
        - name: ENABLE_IPv6
          value: "false"
        securityContext:
          privileged: true
        volumeMounts:
        - mountPath: /host/opt/cni/bin
          name: cni-bin-dir
      containers:
      - name: aws-node
        image: {{.CNIImage}}
        imagePullPolicy: IfNotPresent
        ports:
        - containerPort: 61678
          name: metrics
        livenessProbe:
          exec:
            command: ["/app/grpc-health-probe", "-addr=:50051", "-connect-timeout=5s", "-rpc-timeout=5s"]
          initialDelaySeconds: 60
          timeoutSeconds: 10
        readinessProbe:
          exec:
            command: ["/app/grpc-health-probe", "-addr=:50051", "-connect-timeout=5s", "-rpc-timeout=5s"]
          initialDelaySeconds: 1
          timeoutSeconds: 10
        env:
        - name: ADDITIONAL_ENI_TAGS
          value: "{}"
        - name: ANNOTATE_POD_IP
          value: "false"
        - name: AWS_VPC_CNI_NODE_PORT_SUPPORT
          value: "true"
        - name: AWS_VPC_ENI_MTU
          value: "9001"
        - name: AWS_VPC_K8S_CNI_CUSTOM_NETWORK_CFG
          value: "false"
        - name: AWS_VPC_K8S_CNI_EXTERNALSNAT
          value: "false"
        - name: AWS_VPC_K8S_CNI_LOGLEVEL
          value: "DEBUG"
        - name: AWS_VPC_K8S_CNI_LOG_FILE
          value: "/host/var/log/aws-routed-eni/ipamd.log"
        - name: AWS_VPC_K8S_CNI_RANDOMIZESNAT
          value: "prng"
        - name: AWS_VPC_K8S_CNI_VETHPREFIX
          value: "eni"
        - name: AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS
          value: "{{.ExcludeSNATCSV}}"
        - name: AWS_VPC_K8S_PLUGIN_LOG_FILE
          value: "/var/log/aws-routed-eni/plugin.log"
        - name: AWS_VPC_K8S_PLUGIN_LOG_LEVEL
          value: "DEBUG"
        - name: DISABLE_METRICS
          value: "false"
        - name: DISABLE_NETWORK_RESOURCE_PROVISIONING
          value: "false"
        - name: ENABLE_IPv4
          value: "true"
        - name: ENABLE_IPv6
          value: "false"
        - name: ENABLE_POD_ENI
          value: "false"
        - name: ENABLE_PREFIX_DELEGATION
          value: "false"
        - name: ENABLE_SUBNET_DISCOVERY
          value: "true"
        - name: NETWORK_POLICY_ENFORCING_MODE
          value: "standard"
        - name: VPC_CNI_VERSION
          value: "{{.CNIVersion}}"
        - name: WARM_ENI_TARGET
          value: "1"
        - name: WARM_PREFIX_TARGET
          value: "1"
        - name: MY_NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: CLUSTER_NAME
          value: "{{.ClusterName}}"
        # Bypass any Pod Identity Agent: use IMDS (the satellite-account instance role).
        - name: AWS_EC2_METADATA_DISABLED
          value: "false"
        - name: AWS_REGION
          value: "{{.Region}}"
        securityContext:
          capabilities:
            add: ["NET_ADMIN", "NET_RAW"]
        volumeMounts:
        - mountPath: /host/opt/cni/bin
          name: cni-bin-dir
        - mountPath: /host/etc/cni/net.d
          name: cni-net-dir
        - mountPath: /host/var/log/aws-routed-eni
          name: log-dir
        - mountPath: /var/run/aws-node
          name: run-dir
        - mountPath: /run/xtables.lock
          name: xtables-lock
      - name: aws-eks-nodeagent
        image: {{.NodeAgentImage}}
        imagePullPolicy: IfNotPresent
        args:
        - --enable-ipv6=false
        - --enable-network-policy=false
        - --enable-cloudwatch-logs=false
        - --enable-policy-event-logs=false
        - --metrics-bind-addr=:8162
        - --health-probe-bind-addr=:8163
        env:
        - name: MY_NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        securityContext:
          capabilities:
            add: ["NET_ADMIN"]
          privileged: true
        volumeMounts:
        - mountPath: /host/opt/cni/bin
          name: cni-bin-dir
        - mountPath: /sys/fs/bpf
          name: bpf-pin-path
        - mountPath: /var/log/aws-routed-eni
          name: log-dir
        - mountPath: /var/run/aws-node
          name: run-dir
      volumes:
      - name: bpf-pin-path
        hostPath:
          path: /sys/fs/bpf
      - name: cni-bin-dir
        hostPath:
          path: /opt/cni/bin
      - name: cni-net-dir
        hostPath:
          path: /etc/cni/net.d
      - name: log-dir
        hostPath:
          path: /var/log/aws-routed-eni
          type: DirectoryOrCreate
      - name: run-dir
        hostPath:
          path: /var/run/aws-node
          type: DirectoryOrCreate
      - name: xtables-lock
        hostPath:
          path: /run/xtables.lock
`
