package handlers

import "testing"

// Tests for the pure layout helpers in org.go:
// childSlot, sizeOfSubtree, childSlotInGrid. These compute the canvas
// grid positions for org-import workspace trees and mirror the TypeScript
// layout functions in canvas-topology.ts (defaultChildSlot, parentMinSize,
// childSlotInGrid). The two sides use slightly different default sizes
// (Go: 240×130, TS: 210×120) so they are tested independently.

// childSlot — 2-column fixed-size grid, one row of child cards.
func TestChildSlot_ZeroIndex(t *testing.T) {
	x, y := childSlot(0)
	// col=0, row=0
	// x = 16 + 0*(240+14) = 16
	// y = 130 + 0*(130+14) = 130
	if x != 16.0 {
		t.Errorf("slot 0 x: got %v, want 16.0", x)
	}
	if y != 130.0 {
		t.Errorf("slot 0 y: got %v, want 130.0", y)
	}
}

func TestChildSlot_SecondColumn(t *testing.T) {
	x, y := childSlot(1)
	// col=1, row=0
	// x = 16 + 1*(240+14) = 16+254 = 270
	// y = 130
	if x != 270.0 {
		t.Errorf("slot 1 x: got %v, want 270.0", x)
	}
	if y != 130.0 {
		t.Errorf("slot 1 y: got %v, want 130.0", y)
	}
}

func TestChildSlot_SecondRow(t *testing.T) {
	x, y := childSlot(2)
	// col=0, row=1
	// x = 16
	// y = 130 + 1*(130+14) = 130+144 = 274
	if x != 16.0 {
		t.Errorf("slot 2 x: got %v, want 16.0", x)
	}
	if y != 274.0 {
		t.Errorf("slot 2 y: got %v, want 274.0", y)
	}
}

func TestChildSlot_ThirdRowFirstColumn(t *testing.T) {
	x, y := childSlot(4)
	// col=0, row=2
	// x = 16
	// y = 130 + 2*(130+14) = 130+288 = 418
	if x != 16.0 {
		t.Errorf("slot 4 x: got %v, want 16.0", x)
	}
	if y != 418.0 {
		t.Errorf("slot 4 y: got %v, want 418.0", y)
	}
}

// sizeOfSubtree — bounding-box computation for org-import layout.
func TestSizeOfSubtree_Leaf(t *testing.T) {
	ws := OrgWorkspace{Name: "leaf"}
	s := sizeOfSubtree(ws)
	// Leaf → childDefaultWidth × childDefaultHeight
	if s.width != 240.0 {
		t.Errorf("leaf width: got %v, want 240.0", s.width)
	}
	if s.height != 130.0 {
		t.Errorf("leaf height: got %v, want 130.0", s.height)
	}
}

func TestSizeOfSubtree_OneChild(t *testing.T) {
	ws := OrgWorkspace{Name: "parent", Children: []OrgWorkspace{{Name: "child"}}}
	s := sizeOfSubtree(ws)
	// 1 child → cols=1, rows=1
	// child subtree = (240, 130)
	// width = 16*2 + 240*1 + 14*0 = 272
	// height = 130 + 130 + 14*0 + 16 = 276
	if s.width != 272.0 {
		t.Errorf("1-child width: got %v, want 272.0", s.width)
	}
	if s.height != 276.0 {
		t.Errorf("1-child height: got %v, want 276.0", s.height)
	}
}

func TestSizeOfSubtree_TwoChildren(t *testing.T) {
	ws := OrgWorkspace{Name: "parent", Children: []OrgWorkspace{
		{Name: "c0"}, {Name: "c1"},
	}}
	s := sizeOfSubtree(ws)
	// 2 children → cols=2, rows=1
	// maxColW = 240, totalRowH = 130
	// width = 16*2 + 240*2 + 14*1 = 32+480+14 = 526
	// height = 130 + 130 + 14*0 + 16 = 276
	if s.width != 526.0 {
		t.Errorf("2-child width: got %v, want 526.0", s.width)
	}
	if s.height != 276.0 {
		t.Errorf("2-child height: got %v, want 276.0", s.height)
	}
}

func TestSizeOfSubtree_ThreeChildren(t *testing.T) {
	ws := OrgWorkspace{Name: "parent", Children: []OrgWorkspace{
		{Name: "c0"}, {Name: "c1"}, {Name: "c2"},
	}}
	s := sizeOfSubtree(ws)
	// 3 children → cols=2 (< 3 so capped at 2), rows=2
	// each child = (240, 130), maxColW=240, rowHeights=[130,130]
	// totalRowH = 130+130 = 260
	// width = 16*2 + 240*2 + 14*1 = 526
	// height = 130 + 260 + 14*1 + 16 = 420
	if s.width != 526.0 {
		t.Errorf("3-child width: got %v, want 526.0", s.width)
	}
	if s.height != 420.0 {
		t.Errorf("3-child height: got %v, want 420.0", s.height)
	}
}

func TestSizeOfSubtree_FourChildren(t *testing.T) {
	ws := OrgWorkspace{Name: "parent", Children: []OrgWorkspace{
		{Name: "c0"}, {Name: "c1"}, {Name: "c2"}, {Name: "c3"},
	}}
	s := sizeOfSubtree(ws)
	// 4 children → cols=2, rows=2
	// width = 16*2 + 240*2 + 14*1 = 526
	// height = 130 + 260 + 14*1 + 16 = 420
	if s.width != 526.0 {
		t.Errorf("4-child width: got %v, want 526.0", s.width)
	}
	if s.height != 420.0 {
		t.Errorf("4-child height: got %v, want %v", s.height, 420.0)
	}
}

func TestSizeOfSubtree_FiveChildren(t *testing.T) {
	ws := OrgWorkspace{Name: "parent", Children: []OrgWorkspace{
		{Name: "c0"}, {Name: "c1"}, {Name: "c2"}, {Name: "c3"}, {Name: "c4"},
	}}
	s := sizeOfSubtree(ws)
	// 5 children → cols=2, rows=3
	// rowHeights = [130, 130, 130], totalRowH = 390
	// width = 16*2 + 240*2 + 14*1 = 526
	// height = 130 + 390 + 14*2 + 16 = 564
	if s.width != 526.0 {
		t.Errorf("5-child width: got %v, want 526.0", s.width)
	}
	if s.height != 564.0 {
		t.Errorf("5-child height: got %v, want 564.0", s.height)
	}
}

func TestSizeOfSubtree_NestedTree(t *testing.T) {
	// Grandparent → [Parent(→ child), leaf]
	// parent subtree (1 child): width=272, height=276
	// grandparent:
	//   children = [parent, leaf]
	//   maxColW = max(272, 240) = 272
	//   cols=2, rows=1
	//   width = 16*2 + 272*2 + 14*1 = 590
	//   height = 130 + max(276, 130) + 14*0 + 16 = 422
	parent := OrgWorkspace{Name: "parent", Children: []OrgWorkspace{{Name: "grandchild"}}}
	ws := OrgWorkspace{Name: "grandparent", Children: []OrgWorkspace{parent, {Name: "leaf"}}}
	s := sizeOfSubtree(ws)
	if s.width != 590.0 {
		t.Errorf("nested width: got %v, want 590.0", s.width)
	}
	if s.height != 422.0 {
		t.Errorf("nested height: got %v, want 422.0", s.height)
	}
}

// childSlotInGrid — sibling-aware slot computation; taller siblings push
// subsequent rows down without displacing the column grid.
func TestChildSlotInGrid_EmptySiblings(t *testing.T) {
	x, y := childSlotInGrid(0, nil)
	x2, y2 := childSlotInGrid(0, []nodeSize{})
	// Both nil and empty slice return the top-left padded origin.
	got1, got2 := struct{ x, y float64 }{x, y}, struct{ x, y float64 }{x2, y2}
	for _, g := range []struct{ x, y float64 }{got1, got2} {
		if g.x != 16.0 || g.y != 130.0 {
			t.Errorf("empty siblings: got (%.0f, %.0f), want (16, 130)", g.x, g.y)
		}
	}
}

func TestChildSlotInGrid_Slot0MatchesDefaultChildSlot(t *testing.T) {
	// With uniform 240×130 siblings, slot 0 should equal childSlot(0).
	sizes := []nodeSize{{width: 240, height: 130}, {width: 240, height: 130}}
	x, y := childSlotInGrid(0, sizes)
	cx, cy := childSlot(0)
	if x != cx || y != cy {
		t.Errorf("uniform siblings slot 0: got (%.0f, %.0f), want childSlot (%.0f, %.0f)", x, y, cx, cy)
	}
}

func TestChildSlotInGrid_Slot1MatchesDefaultChildSlot(t *testing.T) {
	sizes := []nodeSize{{width: 240, height: 130}, {width: 240, height: 130}}
	x, y := childSlotInGrid(1, sizes)
	cx, cy := childSlot(1)
	if x != cx || y != cy {
		t.Errorf("uniform siblings slot 1: got (%.0f, %.0f), want childSlot (%.0f, %.0f)", x, y, cx, cy)
	}
}

func TestChildSlotInGrid_TallerSiblingBumpsNextRow(t *testing.T) {
	// Sibling at index 1 is taller (height=300 vs 130).
	// Slot 0: col=0, row=0 → x=16, y=130
	// Slot 1: col=1, row=0 → x=270, y=130
	// Slot 2: col=0, row=1 → x=16, y = 130 + 300 + 14 = 444
	sizes := []nodeSize{
		{width: 240, height: 130},
		{width: 240, height: 300}, // taller — pushes row 2 down
		{width: 240, height: 130},
	}
	x0, y0 := childSlotInGrid(0, sizes)
	if x0 != 16.0 || y0 != 130.0 {
		t.Errorf("slot 0: got (%.0f, %.0f), want (16, 130)", x0, y0)
	}

	x1, y1 := childSlotInGrid(1, sizes)
	if x1 != 270.0 || y1 != 130.0 {
		t.Errorf("slot 1: got (%.0f, %.0f), want (270, 130)", x1, y1)
	}

	x2, y2 := childSlotInGrid(2, sizes)
	// y = parentHeaderPadding + rowHeights[0] + childGutter
	// rowHeights[0] = max(130, 300) = 300
	// y = 130 + 300 + 14 = 444
	if x2 != 16.0 || y2 != 444.0 {
		t.Errorf("slot 2: got (%.0f, %.0f), want (16, 444) — taller sibling pushed row down", x2, y2)
	}
}

func TestChildSlotInGrid_UniformWideSiblingSetsColumnWidth(t *testing.T) {
	// Sibling at index 0 is wider (300 vs 240).
	// Slot 0: x=16, y=130
	// Slot 1: col=1 → x = 16 + 300 + 14 = 330 (NOT 270 = 16+240+14)
	//          y=130
	sizes := []nodeSize{
		{width: 300, height: 130}, // wider — sets column width
		{width: 240, height: 130},
	}
	x1, y1 := childSlotInGrid(1, sizes)
	if x1 != 330.0 || y1 != 130.0 {
		t.Errorf("slot 1: got (%.0f, %.0f), want (330, 130) — col width set by wider sibling", x1, y1)
	}
}

func TestChildSlotInGrid_Slot3OverflowToSecondRow(t *testing.T) {
	// 4 siblings in 2-column grid → rows=2
	// Slot 0: col=0, row=0
	// Slot 1: col=1, row=0
	// Slot 2: col=0, row=1
	// Slot 3: col=1, row=1
	sizes := []nodeSize{
		{width: 240, height: 130},
		{width: 240, height: 130},
		{width: 240, height: 130},
		{width: 240, height: 130},
	}
	x3, y3 := childSlotInGrid(3, sizes)
	// y = 130 + 130 + 14 = 274
	if x3 != 270.0 || y3 != 274.0 {
		t.Errorf("slot 3: got (%.0f, %.0f), want (270, 274)", x3, y3)
	}
}

func TestChildSlotInGrid_MixedSizesCorrectRowAccumulation(t *testing.T) {
	// 3 siblings: [short(130), tall(300), medium(200)]
	// cols=2, rows=2
	// rowHeights[0] = max(130, 300) = 300
	// rowHeights[1] = max(200, 0) = 200
	// slot 0: col=0, row=0 → x=16, y=130
	// slot 1: col=1, row=0 → x=330, y=130
	// slot 2: col=0, row=1 → x=16, y=130+300+14=444
	sizes := []nodeSize{
		{width: 240, height: 130},
		{width: 240, height: 300},
		{width: 240, height: 200},
	}
	x2, y2 := childSlotInGrid(2, sizes)
	if x2 != 16.0 || y2 != 444.0 {
		t.Errorf("slot 2: got (%.0f, %.0f), want (16, 444)", x2, y2)
	}
}
