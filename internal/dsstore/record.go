package dsstore

import (
	"bytes"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// fourCC is the 4-byte identifier the format uses for both a record's
// structure code (`Iloc`, `icvp`) and its payload type (`blob`, `long`).
type fourCC [4]byte

// fcc builds a fourCC from a 4-character string. A shorter string is padded
// with NULs and a longer one is truncated, so callers pass literals only.
func fcc(s string) fourCC {
	var c fourCC
	copy(c[:], s)
	return c
}

func (c fourCC) String() string { return string(c[:]) }

// The structure codes this package writes.
var (
	codeFwi0 = fcc("fwi0") // Finder window info: the content rectangle and view type.
	codeFwsw = fcc("fwsw") // Sidebar width.
	codeFwvh = fcc("fwvh") // Window height.
	codeIcvl = fcc("icvl") // Icon view layout, which modern Finder wants alongside `icvp`.
	codeIcvp = fcc("icvp") // Icon view properties, a binary plist.
	codeIloc = fcc("Iloc") // An item's icon position.
	codeVSrn = fcc("vSrn") // Format version.
	codeVstl = fcc("vstl") // View style.
)

// viewIcon is the `vstl` and `icvl` value that selects the icon view.
var viewIcon = fcc("icnv")

// recordValue is a record's typed payload. The format tags every payload with
// a type code and derives its length from that code, so the four
// implementations below are the closed set this package needs; a value's own
// type carries its encoding rather than a `any` plus a type switch.
type recordValue interface {
	// typeCode is the payload's 4-byte type tag.
	typeCode() fourCC
	// size is the encoded length of the payload, excluding the type tag.
	size() int
	// write appends the payload, excluding the type tag.
	write(buf *bytes.Buffer)
}

// longValue is a `long`: a big-endian uint32.
type longValue uint32

func (longValue) typeCode() fourCC { return fcc("long") }
func (longValue) size() int        { return 4 }
func (v longValue) write(buf *bytes.Buffer) {
	writeU32(buf, uint32(v))
}

// shortValue is a `shor`. Despite the name it occupies four bytes, with the
// value in the low two.
type shortValue uint16

func (shortValue) typeCode() fourCC { return fcc("shor") }
func (shortValue) size() int        { return 4 }
func (v shortValue) write(buf *bytes.Buffer) {
	writeU16(buf, 0)
	writeU16(buf, uint16(v))
}

// typeValue is a `type`: a fourCC payload.
type typeValue fourCC

func (typeValue) typeCode() fourCC { return fcc("type") }
func (typeValue) size() int        { return 4 }
func (v typeValue) write(buf *bytes.Buffer) {
	buf.Write(v[:])
}

// blobValue is a `blob`: a uint32 length followed by that many bytes.
type blobValue []byte

func (blobValue) typeCode() fourCC { return fcc("blob") }
func (v blobValue) size() int      { return 4 + len(v) }
func (v blobValue) write(buf *bytes.Buffer) {
	writeU32(buf, uint32(len(v)))
	buf.Write(v)
}

// record is one entry in the B-tree: it attaches a property to the item named
// by name. A property of the directory itself, such as the window rectangle,
// is stored under the name ".".
type record struct {
	name  string
	code  fourCC
	value recordValue
}

// less orders two records the way Finder expects to find them, which is by
// case-insensitive name and then by structure code. Finder binary-searches a
// node, so a node in any other order hides records from it.
func (r record) less(other record) bool {
	a, b := strings.ToLower(r.name), strings.ToLower(other.name)
	if a != b {
		return a < b
	}
	return r.code.String() < other.code.String()
}

// size is the encoded length of the whole record.
func (r record) size() int {
	return 4 + 2*len(utf16.Encode([]rune(r.name))) + 4 + 4 + r.value.size()
}

// write appends the record: its name, its structure code, then its typed
// payload.
func (r record) write(buf *bytes.Buffer) {
	units := utf16.Encode([]rune(r.name))
	writeU32(buf, uint32(len(units)))
	for _, u := range units {
		writeU16(buf, u)
	}
	buf.Write(r.code[:])
	tc := r.value.typeCode()
	buf.Write(tc[:])
	r.value.write(buf)
}

// The unset values an `Iloc` record carries alongside a position.
const (
	noIndex = 0xFFFFFFFF
	noLabel = 0xFFFF
)

// ilocRecord positions the icon of the item named name. x and y are the
// icon's centre, in points from the window's top-left corner.
func ilocRecord(name string, x, y int) record {
	blob := make([]byte, 16)
	binary.BigEndian.PutUint32(blob[0:4], uint32(x))
	binary.BigEndian.PutUint32(blob[4:8], uint32(y))
	// The item's index within the window, and the label colour after it. An
	// icon view Finder has not been asked to sort leaves both unset, which it
	// spells as all bits set. The final two bytes are padding.
	binary.BigEndian.PutUint32(blob[8:12], noIndex)
	binary.BigEndian.PutUint16(blob[12:14], noLabel)
	return record{name: name, code: codeIloc, value: blobValue(blob)}
}

// fwi0Record holds the window's content rectangle, measured from the top left
// of the screen, and the view type to open it with.
func fwi0Record(top, left, bottom, right int, view fourCC) record {
	blob := make([]byte, 16)
	binary.BigEndian.PutUint16(blob[0:2], uint16(top))
	binary.BigEndian.PutUint16(blob[2:4], uint16(left))
	binary.BigEndian.PutUint16(blob[4:6], uint16(bottom))
	binary.BigEndian.PutUint16(blob[6:8], uint16(right))
	copy(blob[8:12], view[:])
	// The final four bytes are Finder's saved scroll position, which is zero
	// for a window that has never been scrolled.
	return record{name: ".", code: codeFwi0, value: blobValue(blob)}
}

func writeU16(buf *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	buf.Write(b[:])
}

func writeU32(buf *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	buf.Write(b[:])
}
