package discovery

import "testing"

func TestAccountIDFromARN(t *testing.T) {
	tests := []struct {
		arn  string
		want string
	}{
		{"arn:aws:eks:us-east-2:820537372947:cluster/main", "820537372947"},
		{"arn:aws-us-gov:eks:us-gov-west-1:111122223333:cluster/x", "111122223333"},
		{"arn:aws:eks:us-east-2::cluster/main", ""},
		{"too:short", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := AccountIDFromARN(tt.arn); got != tt.want {
			t.Errorf("AccountIDFromARN(%q) = %q, want %q", tt.arn, got, tt.want)
		}
	}
}
