package bootstrap

import (
	"os"
	"testing"
)

func TestNodeadmAlreadyRan(t *testing.T) {
	// NodeadmAlreadyRan checks for /var/lib/kubelet/kubeconfig.
	// On a developer machine, that file should not exist (or if it does, this test
	// would still pass — it's just the inverse).
	got := NodeadmAlreadyRan()
	_, err := os.Stat("/var/lib/kubelet/kubeconfig")
	want := err == nil

	if got != want {
		t.Errorf("NodeadmAlreadyRan() = %v, want %v (kubeconfig exists: %v)", got, want, want)
	}
}
