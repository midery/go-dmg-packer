package dsstore

import (
	"bytes"
	"cmp"
	"fmt"
	"math/bits"
	"slices"
)

// This file writes the `Bud1` container that wraps the records.
//
// A `.DS_Store` is a block store managed by a buddy allocator, and the blocks
// hold a B-tree of records. Three blocks are enough for a layout this small:
//
//	block 0  the allocator's own bookkeeping: every block's address, the
//	         directory list that names the B-tree, and the free lists
//	block 1  the `DSDB` master block, which names the B-tree's root
//	block 2  the root node, a single leaf holding every record
//
// The tree never grows past that one leaf, so there is no code here for
// internal nodes or for splitting: [encodeContainer] fails instead, which
// turns a layout that outgrows a node into a build error rather than a file
// Finder reads only half of.
const (
	// pageSize is the B-tree node size, and the value the master block
	// declares. Finder may ignore a node's records if its block is smaller,
	// so a node is padded to a full page.
	pageSize = 0x1000

	// The `Bud1` header sits at the front of the allocator's address space.
	// The allocator does not own it, but the free lists have to account for
	// it, so it carries a size class like a block.
	headerSizeLog2 = 5
	headerSize     = 1 << headerSizeLog2

	// The block 0 allocation. It is far more than a layout this small needs.
	bookkeepingSizeLog2 = 11
	bookkeepingSize     = 1 << bookkeepingSizeLog2

	// minBlockSizeLog2 is the allocator's smallest size class.
	minBlockSizeLog2 = 5

	// addressSpaceLog2 is the size of the allocator's virtual address space.
	// Finder uses 2^31. The space extends past the end of the file: the
	// blocks past it are free, and allocating one would extend the file.
	addressSpaceLog2 = 31

	// The block ids, in the order the blocks are laid out.
	bookkeepingBlockID = 0
	masterBlockID      = 1
	rootNodeBlockID    = 2
	blockCount         = 3

	// addressBias converts an allocator address to a file offset. An address
	// is relative to the `Bud1` header, which starts 4 bytes into the file,
	// after the alignment field.
	addressBias = 4

	// treeName is the directory the allocator's directory list maps to the
	// master block. Finder looks the B-tree up by this name.
	treeName = "DSDB"
)

// encodeContainer returns the complete `.DS_Store` bytes holding records.
// The records may arrive in any order; they are sorted into the order Finder
// searches for them.
func encodeContainer(records []record) ([]byte, error) {
	sorted := slices.Clone(records)
	slices.SortFunc(sorted, func(a, b record) int {
		switch {
		case a.less(b):
			return -1
		case b.less(a):
			return 1
		default:
			return 0
		}
	})

	node, err := encodeLeafNode(sorted)
	if err != nil {
		return nil, err
	}
	master := encodeMasterBlock(uint32(len(sorted)))

	// Place the blocks. Each one is aligned to its own size, so the blocks
	// leave gaps, and the gaps are what the free lists end up describing.
	// The header holds the front of the space, so the first block follows it.
	layouts := make([]blockLayout, blockCount)
	next := uint32(headerSize)
	for _, b := range [...]struct {
		id   int
		data []byte
	}{{masterBlockID, master}, {rootNodeBlockID, node}} {
		l := blockLayout{sizeLog2: sizeClassFor(len(b.data))}
		l.offset = l.alignUp(next)
		layouts[b.id] = l
		next = l.end()
	}
	bk := blockLayout{sizeLog2: bookkeepingSizeLog2}
	bk.offset = bk.alignUp(next)
	layouts[bookkeepingBlockID] = bk

	// The header is not a block, but it occupies the space in front of them,
	// so the free lists have to see it too.
	occupied := append([]blockLayout{{offset: 0, sizeLog2: headerSizeLog2}}, layouts...)
	bookkeeping := encodeBookkeeping(layouts, freeLists(occupied))
	if len(bookkeeping) > bookkeepingSize {
		return nil, fmt.Errorf("dsstore: the bookkeeping block needs %d bytes, but only %d are allocated for it",
			len(bookkeeping), bookkeepingSize)
	}

	file := make([]byte, addressBias+bk.end())

	// The file opens with an alignment field of 1, then the header.
	file[3] = 0x01
	hdr := new(bytes.Buffer)
	hdr.WriteString("Bud1")
	writeU32(hdr, bk.offset)
	writeU32(hdr, bk.size())
	// The bookkeeping block's address is stored twice; Finder checks the copy
	// against the first.
	writeU32(hdr, bk.offset)
	// Finder reads the root node's address from here rather than walking the
	// directory list to find it.
	writeU32(hdr, layouts[rootNodeBlockID].packed())
	hdr.Write(make([]byte, headerSize-hdr.Len()))
	copy(file[addressBias:], hdr.Bytes())

	copy(file[addressBias+layouts[masterBlockID].offset:], master)
	copy(file[addressBias+layouts[rootNodeBlockID].offset:], node)
	copy(file[addressBias+bk.offset:], bookkeeping)
	return file, nil
}

// encodeLeafNode serializes every record into one leaf node, padded to a full
// page. It fails if they do not fit, because this package writes no internal
// nodes to hang a second leaf off.
func encodeLeafNode(records []record) ([]byte, error) {
	// A node opens with its mode and its record count. A mode of zero marks a
	// leaf; on an internal node the field holds a child block id instead.
	const nodeHeaderSize = 8

	used := nodeHeaderSize
	for _, r := range records {
		used += r.size()
	}
	if used > pageSize {
		return nil, fmt.Errorf("dsstore: %d records need %d bytes, which overflows the %d-byte node this package writes",
			len(records), used, pageSize)
	}

	buf := bytes.NewBuffer(make([]byte, 0, pageSize))
	writeU32(buf, 0)
	writeU32(buf, uint32(len(records)))
	for _, r := range records {
		r.write(buf)
	}
	buf.Write(make([]byte, pageSize-buf.Len()))
	return buf.Bytes(), nil
}

// encodeMasterBlock serializes the `DSDB` master block, which names the root
// node and describes the tree hanging off it.
func encodeMasterBlock(records uint32) []byte {
	buf := new(bytes.Buffer)
	writeU32(buf, rootNodeBlockID)
	// A height of zero means the root is a leaf, and so the tree holds one
	// node.
	writeU32(buf, 0)
	writeU32(buf, records)
	writeU32(buf, 1)
	writeU32(buf, pageSize)
	return buf.Bytes()
}

// encodeBookkeeping serializes block 0: the address of every block, the
// directory list naming the B-tree, and the allocator's free lists.
func encodeBookkeeping(layouts []blockLayout, free [addressSpaceLog2 + 1][]uint32) []byte {
	buf := new(bytes.Buffer)
	writeU32(buf, uint32(len(layouts)))
	// A field Finder writes as zero and whose meaning is not documented.
	writeU32(buf, 0)

	// The offset table is padded to a whole multiple of 256 entries.
	const tableStride = 256
	padded := (len(layouts) + tableStride - 1) / tableStride * tableStride
	for i := 0; i < padded; i++ {
		if i < len(layouts) {
			writeU32(buf, layouts[i].packed())
		} else {
			writeU32(buf, 0)
		}
	}

	// The directory list. This package writes the one entry Finder needs.
	writeU32(buf, 1)
	buf.WriteByte(byte(len(treeName)))
	buf.WriteString(treeName)
	writeU32(buf, masterBlockID)

	// One free list per size class, each a count followed by the address of
	// every free block of that size. The classes run from 2^0 up, and the
	// table always holds all 32 of them.
	for _, list := range free {
		writeU32(buf, uint32(len(list)))
		for _, offset := range list {
			writeU32(buf, offset)
		}
	}
	return buf.Bytes()
}

// blockLayout is where something sits in the allocator's address space and
// which size class it was given. Sizes are held as exponents because that is
// how the format stores them, and because it makes the alignment rule — a
// block starts on a boundary of its own size — a property of the pair.
type blockLayout struct {
	offset   uint32
	sizeLog2 uint32
}

func (l blockLayout) size() uint32 { return 1 << l.sizeLog2 }

func (l blockLayout) end() uint32 { return l.offset + l.size() }

// alignUp returns the first address at or after from that this block may
// start at.
func (l blockLayout) alignUp(from uint32) uint32 {
	mask := l.size() - 1
	return (from + mask) & ^mask
}

// packed is how an address appears in the offset table and in the header. A
// block starts on a boundary of its own size, so the low bits of its offset
// are free to carry the exponent.
func (l blockLayout) packed() uint32 {
	return l.offset | l.sizeLog2
}

// sizeClassFor returns the exponent of the smallest allocation that holds n
// bytes.
func sizeClassFor(n int) uint32 {
	if n <= 1<<minBlockSizeLog2 {
		return minBlockSizeLog2
	}
	// Len32 of n-1 is the position of its highest set bit, which is the
	// exponent to round n up to.
	return uint32(bits.Len32(uint32(n) - 1))
}

// freeLists describes every part of the address space that occupied does not
// cover, as the allocator's per-size-class lists.
//
// A free block has to be a power of two in size and start on a boundary of
// that size, so describing a gap means cutting it into such blocks. Doing
// that from the gap's low end, taking the largest block that both starts on
// its own boundary and still fits, is the only way to cut it: at each step
// the address itself caps how large a block may start there, so no choice
// arises. The result comes out in increasing address order, which is the
// order Finder writes.
func freeLists(occupied []blockLayout) [addressSpaceLog2 + 1][]uint32 {
	var lists [addressSpaceLog2 + 1][]uint32

	// Cut the gap [from, to) into free blocks.
	cut := func(from, to uint32) {
		for from < to {
			// How large a block may start at this address, and how much of
			// the gap is left to put one in.
			startCap := uint32(bits.TrailingZeros32(from))
			roomCap := uint32(bits.Len32(to-from)) - 1
			sizeLog2 := min(startCap, roomCap, addressSpaceLog2)

			lists[sizeLog2] = append(lists[sizeLog2], from)
			from += uint32(1) << sizeLog2
		}
	}

	inOrder := slices.Clone(occupied)
	slices.SortFunc(inOrder, func(a, b blockLayout) int {
		return cmp.Compare(a.offset, b.offset)
	})

	at := uint32(0)
	for _, l := range inOrder {
		cut(at, l.offset)
		at = l.end()
	}
	// Everything past the last block is free, out to the end of the space.
	// That tail is most of it: the space is far larger than the file, and
	// allocating from the tail is what would extend the file.
	cut(at, 1<<addressSpaceLog2)

	return lists
}
