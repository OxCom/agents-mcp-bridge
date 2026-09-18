package run

import "testing"

// TestTheFirstVendorFailureWins keeps the cause rather than a consequence: a
// vendor emitting a second failure event must not overwrite the reason the
// caller needs.
func TestTheFirstVendorFailureWins(t *testing.T) {
	r := NewForTest("r3")
	r.noteVendorFailure("")
	r.noteVendorFailure("usage limit reached")
	r.noteVendorFailure("stream closed")

	if got := r.VendorFailure(); got != "usage limit reached" {
		t.Fatalf("VendorFailure = %q", got)
	}
}
