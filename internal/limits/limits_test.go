package limits

import "testing"

// TestLimitsValuesArePinned guards the four shared byte caps against
// accidental drift. These values are baked into operator alerting, dashboard
// thresholds, and the wire-format contract: changing them requires a
// breaking-change CHANGELOG entry and coordinated release notes. This test is
// a forcing function to make accidental drift loud — if you intentionally
// changed a value, update the constant here in the same commit and bump the
// CHANGELOG with the rationale.
func TestLimitsValuesArePinned(t *testing.T) {
	if MaxDeviceIDBytes != 256 {
		t.Errorf("MaxDeviceIDBytes = %d, want 256", MaxDeviceIDBytes)
	}
	if MaxPortNameBytes != 256 {
		t.Errorf("MaxPortNameBytes = %d, want 256", MaxPortNameBytes)
	}
	if MaxLabelKeyBytes != 256 {
		t.Errorf("MaxLabelKeyBytes = %d, want 256", MaxLabelKeyBytes)
	}
	if MaxLabelValueBytes != 4096 {
		t.Errorf("MaxLabelValueBytes = %d, want 4096", MaxLabelValueBytes)
	}
	if MaxIfIndex != 2147483647 {
		t.Errorf("MaxIfIndex = %d, want 2147483647", MaxIfIndex)
	}
}
