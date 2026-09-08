// Package dsstore composes the Finder `.DS_Store` that styles the installer
// window of a disk image.
//
// The package holds both the layout — the window rectangle, the icon view
// options, and where the two icons sit — and the encoder for the format
// itself, which is a buddy-allocated block store holding a B-tree of
// records. See `bud1.go` for the container and `record.go` for the records.
// Only the records this layout needs are implemented.
//
// Finder keys an icon's position on the item's file name, so the layout
// cannot be bundle-name-agnostic on its own and [Encode] takes the name.
//
// "Write-once" like the rest of this tool: [Encode] composes one complete
// file. There is no support for reading or editing an existing `.DS_Store`.
package dsstore

import (
	"fmt"
	"strings"

	"howett.net/plist"
)

// The drag-to-install pair, in points from the window's top-left corner.
// Finder reads an `Iloc` position as the icon's centre, so these are centres
// too. The values mirror `makedmg.py`, the macOS-host path this replaces.
const (
	AppIconX = 110
	AppIconY = 167

	AliasIconX = 340
	AliasIconY = 167
)

// AliasName is the `/Applications` symlink's name in the volume root, and so
// the name its icon position is keyed on.
const AliasName = "Applications"

// The installer window's rectangle on screen.
const (
	windowTop    = 100
	windowLeft   = 400
	windowBottom = 396
	windowRight  = 855
)

// The icon view options Finder stores in the `icvp` record. Sizes are in
// points.
const (
	iconSize    = 100
	textSize    = 12
	gridSpacing = 100
)

// sidebarWidth is the `fwsw` record. The window hides its sidebar, so this is
// the width Finder restores if the viewer turns the sidebar back on.
const sidebarWidth = 170

// hiddenNames are the items Finder parks outside the window so they stay
// invisible when the viewer has hidden files turned on. They are named even
// though a read-only volume has no `.fseventsd` or `.Trashes`, because
// `hdiutil` names them and an unused record is cheap.
var hiddenNames = []string{".background", ".DS_Store", ".fseventsd", ".Trashes"}

// hiddenIconY is the row the parked icons sit on, and hiddenIconX the first
// column. Each subsequent item steps right by hiddenIconStep, well past the
// window's right edge.
const (
	hiddenIconX    = 560
	hiddenIconY    = 170
	hiddenIconStep = 50
)

// Encode returns the `.DS_Store` bytes that style a volume root holding
// appEntry, the application bundle's name including its `.app` extension.
func Encode(appEntry string) ([]byte, error) {
	if appEntry == "" {
		return nil, fmt.Errorf("dsstore: the application bundle name is empty")
	}
	if !strings.HasSuffix(appEntry, ".app") {
		return nil, fmt.Errorf("dsstore: %q is not an `.app` bundle name", appEntry)
	}

	iconView, err := iconViewProperties()
	if err != nil {
		return nil, err
	}

	records := []record{
		// Write the legacy `fwi0` record and no `bwsp` record. Finder prefers `bwsp`, the modern window plist,
		// whenever it is present. `bwsp` holds the window frame in Cocoa coordinates, which start at the bottom left
		// of the screen, and the frame includes the title bar. A rectangle baked in at build time therefore puts the
		// window at a height that depends on the screen, and it leaves the icon area 50 points too short. The two
		// icons then touch the bottom edge. `fwi0` holds the content rectangle from the top left of the screen, so it
		// gives the same window on every screen. It is also what `makedmg.py` writes, the macOS-host path this tool
		// replaces.
		//
		// Do not add a `bwsp` record.
		fwi0Record(windowTop, windowLeft, windowBottom, windowRight, viewIcon),
		{name: ".", code: codeFwsw, value: longValue(sidebarWidth)},
		{name: ".", code: codeFwvh, value: shortValue(windowBottom - windowTop)},
		{name: ".", code: codeVstl, value: typeValue(viewIcon)},
		{name: ".", code: codeIcvp, value: blobValue(iconView)},
		// Modern Finder wants the view named again as a type code beside the plist.
		{name: ".", code: codeIcvl, value: typeValue(viewIcon)},
		// The format version. Finder treats a store without it as unreadable.
		{name: ".", code: codeVSrn, value: longValue(1)},

		ilocRecord(appEntry, AppIconX, AppIconY),
		ilocRecord(AliasName, AliasIconX, AliasIconY),
	}
	for i, name := range hiddenNames {
		records = append(records, ilocRecord(name, hiddenIconX+i*hiddenIconStep, hiddenIconY))
	}

	return encodeContainer(records)
}

// iconViewProperties returns the binary plist that goes in the `icvp` record.
//
// The keys are the ones Finder writes itself, and it is unforgiving about
// them: an unexpected value makes it discard the whole plist and fall back to
// its own defaults, which looks like the record never arrived. In particular a
// `gridSpacing` above 100 loses every icon view setting, and a
// `backgroundType` of 0 does the same, so a white ground is spelled out as a
// colour instead.
//
// The distinction between an integer and a real matters here too, because it
// survives into the encoding, so the numbers below are typed deliberately.
func iconViewProperties() ([]byte, error) {
	data, err := plist.Marshal(map[string]any{
		"viewOptionsVersion": 1,
		"iconSize":           float64(iconSize),
		"textSize":           float64(textSize),
		"gridSpacing":        float64(gridSpacing),
		"gridOffsetX":        float64(0),
		"gridOffsetY":        float64(0),
		"arrangeBy":          "none",
		"scrollPositionX":    float64(0),
		"scrollPositionY":    float64(0),
		"labelOnBottom":      true,
		// The window shows one bundle and one alias, so an icon preview is the app icon and item info is noise.
		"showIconPreview": true,
		"showItemInfo":    false,
		// A plain white ground. `/.background/` is staged but empty, so a picture record would point at nothing.
		"backgroundType":       1,
		"backgroundColorRed":   float64(1),
		"backgroundColorGreen": float64(1),
		"backgroundColorBlue":  float64(1),
	}, plist.BinaryFormat)
	if err != nil {
		return nil, fmt.Errorf("dsstore: encode the icon view properties: %w", err)
	}
	return data, nil
}
