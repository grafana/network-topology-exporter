package discovery

import "testing"

func TestInferSessionType(t *testing.T) {
	if got := InferSessionType("100", "100", ""); got != SessionTypeIBGP {
		t.Fatalf("equal AS: got %q", got)
	}
	if got := InferSessionType("100", "200", ""); got != SessionTypeEBGP {
		t.Fatalf("unequal AS: got %q", got)
	}
	if got := InferSessionType("", "", "iBGP-overlay"); got != SessionTypeIBGP {
		t.Fatalf("peer group: got %q", got)
	}
	if got := InferSessionType("", "", ""); got != "" {
		t.Fatalf("empty: got %q", got)
	}
}
