package dsstore

import (
	"bytes"
	"os"
	"testing"
)

// TestEncodeMatchesGolden pins the output byte for byte.
//
// The fixtures are output that has been checked against real Finder, so
// reproducing them exactly is what keeps the encoder honest.
//
// A deliberate layout change means regenerating the fixtures, and it means
// mounting the result and looking at the window before doing so.
func TestEncodeMatchesGolden(t *testing.T) {
	for _, appEntry := range []string{"Air.app", "Fleet.app"} {
		t.Run(appEntry, func(t *testing.T) {
			want, err := os.ReadFile("testdata/" + appEntry + ".DS_Store")
			if err != nil {
				t.Fatalf("read the fixture: %v", err)
			}
			got, err := Encode(appEntry)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if bytes.Equal(got, want) {
				return
			}
			if len(got) != len(want) {
				t.Fatalf("length: got %d want %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("first difference at byte %d: got 0x%02x want 0x%02x", i, got[i], want[i])
				}
			}
		})
	}
}
