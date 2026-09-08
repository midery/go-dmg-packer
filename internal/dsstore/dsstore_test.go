package dsstore

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func TestEncodeWritesABud1Container(t *testing.T) {
	got, err := Encode("Air.app")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(got) < 32 {
		t.Fatalf("length: got %d, too short to hold a header", len(got))
	}
	if string(got[4:8]) != "Bud1" {
		t.Errorf("signature bytes: got %q want \"Bud1\"", got[4:8])
	}
}

// TestEncodePositionsTheDragToInstallPair is the point of the package. The
// two icons must land on the coordinates the layout names, because Finder
// auto-places anything it has no `Iloc` record for.
func TestEncodePositionsTheDragToInstallPair(t *testing.T) {
	got, err := Encode("Air.app")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	positions := iconPositions(t, got)

	want := map[string][2]int{
		"Air.app":     {AppIconX, AppIconY},
		AliasName:     {AliasIconX, AliasIconY},
		".background": {hiddenIconX, hiddenIconY},
		".DS_Store":   {hiddenIconX + hiddenIconStep, hiddenIconY},
		".fseventsd":  {hiddenIconX + 2*hiddenIconStep, hiddenIconY},
		".Trashes":    {hiddenIconX + 3*hiddenIconStep, hiddenIconY},
	}
	for name, at := range want {
		got, ok := positions[name]
		if !ok {
			t.Errorf("%q: no `Iloc` record", name)
			continue
		}
		if got != at {
			t.Errorf("%q: got (%d, %d) want (%d, %d)", name, got[0], got[1], at[0], at[1])
		}
	}
	if len(positions) != len(want) {
		t.Errorf("`Iloc` record count: got %d want %d (%v)", len(positions), len(want), positions)
	}
}

// TestEncodeUsesTheBundleName guards what the pre-built template got wrong:
// it hard-coded `Air.app`, so any other product got a window with no layout.
func TestEncodeUsesTheBundleName(t *testing.T) {
	got, err := Encode("Fleet.app")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, ok := iconPositions(t, got)["Fleet.app"]; !ok {
		t.Errorf("no `Iloc` record for \"Fleet.app\"")
	}
	if bytes.Contains(got, utf16BE("Air.app")) {
		t.Errorf("the output still names \"Air.app\"")
	}
}

// TestEncodeIsDeterministic guards the reproducible build. The upstream
// package sorts its records, but nothing in its API promises a stable
// encoding, so assert it rather than assume it.
func TestEncodeIsDeterministic(t *testing.T) {
	first, err := Encode("Fleet.app")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	second, err := Encode("Fleet.app")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("two runs disagree")
	}
}

// TestEncodeWritesTheWindowRecords checks that the window rectangle and the
// icon view options reached the file. Finder falls back to its own defaults
// for any record that is absent, which looks like nothing went wrong.
func TestEncodeWritesTheWindowRecords(t *testing.T) {
	got, err := Encode("Air.app")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, id := range []string{"fwi0", "icvp", "vstl", "fwsw", "fwvh"} {
		if !bytes.Contains(got, []byte(id)) {
			t.Errorf("no %q record", id)
		}
	}
	// Finder prefers `bwsp` over `fwi0`, and a `bwsp` rectangle cannot be correct on every screen. See [Encode].
	if bytes.Contains(got, []byte("bwsp")) {
		t.Errorf("the output holds a `bwsp` record")
	}
	// `fwi0` stores the rectangle as four big-endian uint16s, then the view style.
	rect := make([]byte, 8)
	binary.BigEndian.PutUint16(rect[0:2], windowTop)
	binary.BigEndian.PutUint16(rect[2:4], windowLeft)
	binary.BigEndian.PutUint16(rect[4:6], windowBottom)
	binary.BigEndian.PutUint16(rect[6:8], windowRight)
	if !bytes.Contains(got, rect) {
		t.Errorf("the window rectangle %v is not in the output", rect)
	}
}

func TestEncodeRejectsABadBundleName(t *testing.T) {
	for _, name := range []string{"", "Air", "Air.APP", "Air.app.zip"} {
		if _, err := Encode(name); err == nil {
			t.Errorf("Encode(%q) returned no error", name)
		}
	}
}

// iconPositions decodes every `Iloc` record in the single leaf node and
// returns the position each one pins its item to.
func iconPositions(t *testing.T, b []byte) map[string][2]int {
	t.Helper()
	out := map[string][2]int{}
	for _, r := range records(t, b) {
		if r.id != "Iloc" {
			continue
		}
		if len(r.payload) < 12 {
			t.Fatalf("%q: an `Iloc` payload of %d bytes is too short", r.name, len(r.payload))
		}
		// A blob payload is length-prefixed, then x and y as big-endian uint32s.
		x := binary.BigEndian.Uint32(r.payload[4:8])
		y := binary.BigEndian.Uint32(r.payload[8:12])
		out[r.name] = [2]int{int(x), int(y)}
	}
	return out
}

type decoded struct {
	name    string
	id      string
	kind    string
	payload []byte
}

// records walks the B-tree and returns every record in it.
//
// The walk resolves each block through the allocator rather than assuming a
// fixed offset, because the block layout is the encoder's choice: a smaller
// record set moves the B-tree node and the allocator root.
func records(t *testing.T, b []byte) []decoded {
	t.Helper()
	if len(b) < 36 {
		t.Fatalf("length: got %d, too short to hold a header", len(b))
	}
	// An address in the allocator is relative to the 4-byte alignment field, so a block at address a starts at a+4.
	const addressBias = 4

	// The header names the allocator root, whose offset table maps a block id to its address.
	allocator := b[int(binary.BigEndian.Uint32(b[8:12]))+addressBias:]
	blockCount := int(binary.BigEndian.Uint32(allocator[0:4]))
	blockAt := func(id int) []byte {
		if id >= blockCount {
			t.Fatalf("block id %d is out of range, the file has %d blocks", id, blockCount)
		}
		// An offset table entry packs the block's address with its buddy order in the low 5 bits.
		packed := binary.BigEndian.Uint32(allocator[8+4*id : 12+4*id])
		return b[int(packed & ^uint32(0x1F))+addressBias:]
	}

	// The allocator's directory list names the master block, which in turn names the root node.
	at := 8 + 4*256
	if dirs := binary.BigEndian.Uint32(allocator[at : at+4]); dirs != 1 {
		t.Fatalf("directory count: got %d want 1", dirs)
	}
	at += 4
	nameLen := int(allocator[at])
	at++
	if name := string(allocator[at : at+nameLen]); name != "DSDB" {
		t.Fatalf("directory name: got %q want \"DSDB\"", name)
	}
	at += nameLen
	master := blockAt(int(binary.BigEndian.Uint32(allocator[at : at+4])))
	node := blockAt(int(binary.BigEndian.Uint32(master[0:4])))

	if mode := binary.BigEndian.Uint32(node[0:4]); mode != 0 {
		t.Fatalf("node mode: got %d want 0, so the root is not a leaf and the layout no longer fits one node", mode)
	}
	count := int(binary.BigEndian.Uint32(node[4:8]))

	var out []decoded
	p := 8
	for i := 0; i < count; i++ {
		units := int(binary.BigEndian.Uint32(node[p : p+4]))
		p += 4
		name := make([]uint16, units)
		for j := range name {
			name[j] = binary.BigEndian.Uint16(node[p : p+2])
			p += 2
		}
		r := decoded{name: string(utf16.Decode(name))}
		r.id = string(node[p : p+4])
		p += 4
		r.kind = string(node[p : p+4])
		p += 4

		var size int
		switch r.kind {
		case "bool":
			size = 1
		case "long", "shor", "type":
			size = 4
		case "comp", "dutc":
			size = 8
		case "blob":
			size = 4 + int(binary.BigEndian.Uint32(node[p:p+4]))
		case "ustr":
			// A `ustr` length is in UTF-16 code units, not bytes.
			size = 4 + 2*int(binary.BigEndian.Uint32(node[p:p+4]))
		default:
			t.Fatalf("record %d (%q): unknown payload type %q", i, r.name, r.kind)
		}
		r.payload = node[p : p+size]
		p += size
		out = append(out, r)
	}
	return out
}

// utf16BE encodes s the way a record name is stored, so a test can look for a
// name in the raw bytes.
func utf16BE(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.BigEndian.PutUint16(b[2*i:2*i+2], u)
	}
	return b
}
