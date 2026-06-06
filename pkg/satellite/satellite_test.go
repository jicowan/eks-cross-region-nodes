package satellite

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func validParams() Params {
	return Params{
		AccountID:        "310444902345",
		Region:           "us-west-1",
		ClusterName:      "main",
		ExcludeSNATCIDRs: []string{"10.0.0.0/16", "10.1.0.0/16", "10.2.0.0/16"},
	}
}

func TestName(t *testing.T) {
	p := validParams()
	if got, want := p.Name(), "aws-node-satellite-310444902345-us-west-1"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Params)
		wantErr bool
	}{
		{"valid", func(p *Params) {}, false},
		{"missing account", func(p *Params) { p.AccountID = "" }, true},
		{"missing region", func(p *Params) { p.Region = "" }, true},
		{"missing cluster", func(p *Params) { p.ClusterName = "" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validParams()
			tt.mutate(&p)
			err := p.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRenderValidatesInput(t *testing.T) {
	_, err := Render(Params{Region: "us-west-1", ClusterName: "main"}) // missing AccountID
	if err == nil {
		t.Fatal("Render should reject params missing AccountID")
	}
}

// docs splits the rendered multi-doc YAML into individual parsed objects.
func docs(t *testing.T, out string) []map[string]interface{} {
	t.Helper()
	var result []map[string]interface{}
	for _, chunk := range strings.Split(out, "\n---\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(chunk), &m); err != nil {
			t.Fatalf("rendered chunk is not valid YAML: %v\n---\n%s", err, chunk)
		}
		if len(m) > 0 {
			result = append(result, m)
		}
	}
	return result
}

func TestRenderProducesThreeValidDocs(t *testing.T) {
	out, err := Render(validParams())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	objs := docs(t, out)
	if len(objs) != 3 {
		t.Fatalf("expected 3 documents (SA, CRB, DS), got %d", len(objs))
	}

	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[o["kind"].(string)] = true
	}
	for _, want := range []string{"ServiceAccount", "ClusterRoleBinding", "DaemonSet"} {
		if !kinds[want] {
			t.Errorf("missing %s in rendered output", want)
		}
	}
}

func TestRenderNamingAndScoping(t *testing.T) {
	out, err := Render(validParams())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	name := "aws-node-satellite-310444902345-us-west-1"

	objs := docs(t, out)
	for _, o := range objs {
		md := o["metadata"].(map[string]interface{})
		if md["name"] != name {
			t.Errorf("%s metadata.name = %v, want %s", o["kind"], md["name"], name)
		}
	}

	// Find the DaemonSet and verify nodeAffinity + key env.
	var ds map[string]interface{}
	for _, o := range objs {
		if o["kind"] == "DaemonSet" {
			ds = o
		}
	}
	if ds == nil {
		t.Fatal("no DaemonSet found")
	}

	podSpec := ds["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})

	if sa := podSpec["serviceAccountName"]; sa != name {
		t.Errorf("serviceAccountName = %v, want %s", sa, name)
	}

	// nodeAffinity must scope by account AND region AND compute-type=hybrid.
	terms := podSpec["affinity"].(map[string]interface{})["nodeAffinity"].(map[string]interface{})["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]interface{})["nodeSelectorTerms"].([]interface{})
	exprs := terms[0].(map[string]interface{})["matchExpressions"].([]interface{})
	got := map[string]string{}
	for _, e := range exprs {
		em := e.(map[string]interface{})
		vals := em["values"].([]interface{})
		got[em["key"].(string)] = vals[0].(string)
	}
	if got["eks.amazonaws.com/compute-type"] != "hybrid" {
		t.Errorf("compute-type affinity = %q, want hybrid", got["eks.amazonaws.com/compute-type"])
	}
	if got["xrn.amazonaws.com/satellite-account"] != "310444902345" {
		t.Errorf("satellite-account affinity = %q", got["xrn.amazonaws.com/satellite-account"])
	}
	if got["topology.kubernetes.io/region"] != "us-west-1" {
		t.Errorf("region affinity = %q", got["topology.kubernetes.io/region"])
	}
}

func TestRenderEnvSubstitution(t *testing.T) {
	out, err := Render(validParams())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// AWS_REGION and SNAT CIDRs are the cross-account-critical env values.
	if !strings.Contains(out, `value: "us-west-1"`) {
		t.Error("AWS_REGION not set to satellite region")
	}
	if !strings.Contains(out, "10.0.0.0/16,10.1.0.0/16,10.2.0.0/16") {
		t.Error("EXCLUDE_SNAT_CIDRS not joined correctly")
	}
	// ClusterRoleBinding must reuse the stock aws-node ClusterRole.
	if !strings.Contains(out, "name: aws-node\n") {
		t.Error("ClusterRoleBinding should reference the aws-node ClusterRole")
	}
}

func TestRenderImageDefaultsAndOverrides(t *testing.T) {
	// Defaults
	out, _ := Render(validParams())
	if !strings.Contains(out, DefaultRegistry+"/amazon-k8s-cni:"+DefaultCNIImageTag) {
		t.Error("default CNI image not rendered")
	}

	// Overrides
	p := validParams()
	p.Registry = "111122223333.dkr.ecr.eu-west-1.amazonaws.com"
	p.CNIImageTag = "v1.99.0-test"
	out, _ = Render(p)
	if !strings.Contains(out, "111122223333.dkr.ecr.eu-west-1.amazonaws.com/amazon-k8s-cni:v1.99.0-test") {
		t.Error("CNI image override not rendered")
	}
	if strings.Contains(out, DefaultRegistry) {
		t.Error("default registry should not appear when overridden")
	}
}
