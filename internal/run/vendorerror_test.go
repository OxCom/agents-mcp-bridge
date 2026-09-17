package run

import "testing"

// TestAnErrorEventWithNoMessageStillCounts pins the occurrence against the
// message. A vendor that emits an error event carrying no text has still
// reported an error, and a caller reading the count must see it.
func TestAnErrorEventWithNoMessageStillCounts(t *testing.T) {
	r := NewForTest("r1")
	r.noteVendorError("")
	r.noteVendorError("something went wrong")
	r.noteVendorError("")

	s := r.Snapshot()
	if s.VendorErrorCount != 3 {
		t.Fatalf("VendorErrorCount = %d, want 3", s.VendorErrorCount)
	}
	if len(s.VendorErrors) != 1 || s.VendorErrors[0] != "something went wrong" {
		t.Fatalf("VendorErrors = %q, want only the one event that had text", s.VendorErrors)
	}
}

// TestVendorErrorOccurrencesRespectTheCap keeps the textless path under the
// same bound as the text one: a vendor looping on an error must not grow the
// run without limit.
func TestVendorErrorOccurrencesRespectTheCap(t *testing.T) {
	r := NewForTest("r2")
	for i := 0; i < maxVendorErrors*3; i++ {
		r.noteVendorError("")
	}
	if got := r.Snapshot().VendorErrorCount; got != maxVendorErrors {
		t.Fatalf("VendorErrorCount = %d, want the cap %d", got, maxVendorErrors)
	}
}
