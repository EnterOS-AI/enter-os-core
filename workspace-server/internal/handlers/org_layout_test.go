package handlers

// org_layout_test.go — unit coverage for org canvas layout helpers
// (org.go). These functions compute canvas node positions and subtree
// bounding boxes; they are pure (no DB calls, no side effects).
//
// Coverage targets:
//   - childSlot: 2-column grid x,y for 0th..Nth child
//   - sizeOfSubtree: leaf, single child, multi-child, deep nesting
//   - childSlotInGrid: empty siblings, uniform sizes, variable sizes,
//     index boundaries

import "testing"

// ---------- childSlot ----------

func TestChildSlot_FirstChild(t *testing.T) {
	x, y := childSlot(0)
	// col=0, row=0; x=parentSidePadding=16, y=parentHeaderPadding=130
	if x != 16.0 {
		t.Errorf("x = %v; want 16.0", x)
	}
	if y != 130.0 {
		t.Errorf("y = %v; want 130.0", y)
	}
}

func TestChildSlot_SecondChild(t *testing.T) {
	x, y := childSlot(1)
	// col=1, row=0; x=16+(240+14)=270, y=130
	if x != 270.0 {
		t.Errorf("x = %v; want 270.0", x)
	}
	if y != 130.0 {
		t.Errorf("y = %v; want 130.0", y)
	}
}

func TestChildSlot_ThirdChild(t *testing.T) {
	x, y := childSlot(2)
	// col=0, row=1; x=16, y=130+(130+14)=274
	if x != 16.0 {
		t.Errorf("x = %v; want 16.0", x)
	}
	if y != 274.0 {
		t.Errorf("y = %v; want 274.0", y)
	}
}

func TestChildSlot_FourthChild(t *testing.T) {
	x, y := childSlot(3)
	// col=1, row=1; x=270, y=274
	if x != 270.0 {
		t.Errorf("x = %v; want 270.0", x)
	}
	if y != 274.0 {
		t.Errorf("y = %v; want 274.0", y)
	}
}

// ---------- sizeOfSubtree ----------

func TestSizeOfSubtree_Leaf(t *testing.T) {
	ws := OrgWorkspace{Name: "leaf"}
	size := sizeOfSubtree(ws)
	if size.width != 240.0 {
		t.Errorf("width = %v; want 240.0", size.width)
	}
	if size.height != 130.0 {
		t.Errorf("height = %v; want 130.0", size.height)
	}
}

func TestSizeOfSubtree_SingleChild(t *testing.T) {
	ws := OrgWorkspace{
		Name:     "parent",
		Children: []OrgWorkspace{{Name: "child"}},
	}
	size := sizeOfSubtree(ws)
	// cols = min(1,1) = 1; rows = 1
	// maxColW = 240 (child default)
	// width = 16*2 + 240*1 + 14*0 = 272
	// height = 130 + 130 + 14*0 + 16 = 276
	if size.width != 272.0 {
		t.Errorf("width = %v; want 272.0", size.width)
	}
	if size.height != 276.0 {
		t.Errorf("height = %v; want 276.0", size.height)
	}
}

func TestSizeOfSubtree_TwoChildren(t *testing.T) {
	ws := OrgWorkspace{
		Name: "parent",
		Children: []OrgWorkspace{
			{Name: "child1"},
			{Name: "child2"},
		},
	}
	size := sizeOfSubtree(ws)
	// cols = 2; rows = 1; maxColW = 240
	// width = 16*2 + 240*2 + 14*1 = 526
	// height = 130 + (130+130) + 14*0 + 16 = 276
	if size.width != 526.0 {
		t.Errorf("width = %v; want 526.0", size.width)
	}
	if size.height != 276.0 {
		t.Errorf("height = %v; want 276.0", size.height)
	}
}

func TestSizeOfSubtree_ThreeChildren(t *testing.T) {
	ws := OrgWorkspace{
		Name: "parent",
		Children: []OrgWorkspace{
			{Name: "child1"},
			{Name: "child2"},
			{Name: "child3"},
		},
	}
	size := sizeOfSubtree(ws)
	// cols = 2 (len=3, childGridColumnCount=2, min=2); rows = 2
	// maxColW = 240
	// width = 16*2 + 240*2 + 14*1 = 526
	// height = 130 + (130*2) + 14*1 + 16 = 420
	if size.width != 526.0 {
		t.Errorf("width = %v; want 526.0", size.width)
	}
	if size.height != 420.0 {
		t.Errorf("height = %v; want 420.0", size.height)
	}
}

func TestSizeOfSubtree_DeepNesting(t *testing.T) {
	// leaf → child → parent
	grandchild := OrgWorkspace{Name: "grandchild"}
	child := OrgWorkspace{Name: "child", Children: []OrgWorkspace{grandchild}}
	parent := OrgWorkspace{Name: "parent", Children: []OrgWorkspace{child}}
	size := sizeOfSubtree(parent)
	// grandchild: 240x130
	// child: cols=1, rows=1, maxColW=240 → 272x276
	// parent: cols=1, rows=1, maxColW=272 → 304x422
	if size.width != 304.0 {
		t.Errorf("width = %v; want 304.0", size.width)
	}
	if size.height != 422.0 {
		t.Errorf("height = %v; want 422.0", size.height)
	}
}

// ---------- childSlotInGrid ----------

func TestChildSlotInGrid_EmptySiblings(t *testing.T) {
	x, y := childSlotInGrid(0, nil)
	if x != 16.0 || y != 130.0 {
		t.Errorf("empty siblings: got (%v,%v); want (16.0, 130.0)", x, y)
	}
}

func TestChildSlotInGrid_EmptySlice(t *testing.T) {
	x, y := childSlotInGrid(0, []nodeSize{})
	if x != 16.0 || y != 130.0 {
		t.Errorf("empty slice: got (%v,%v); want (16.0, 130.0)", x, y)
	}
}

func TestChildSlotInGrid_UniformSizes(t *testing.T) {
	sizes := []nodeSize{
		{240, 130},
		{240, 130},
		{240, 130},
	}
	// maxColW = 240; cols = 2; rows = 2
	// slot 0: col=0, row=0 → x=16, y=130
	x0, y0 := childSlotInGrid(0, sizes)
	if x0 != 16.0 || y0 != 130.0 {
		t.Errorf("slot 0: got (%v,%v); want (16.0, 130.0)", x0, y0)
	}
	// slot 1: col=1, row=0 → x=16+240+14=270, y=130
	x1, y1 := childSlotInGrid(1, sizes)
	if x1 != 270.0 || y1 != 130.0 {
		t.Errorf("slot 1: got (%v,%v); want (270.0, 130.0)", x1, y1)
	}
	// slot 2: col=0, row=1 → x=16, y=130+130+14=274
	x2, y2 := childSlotInGrid(2, sizes)
	if x2 != 16.0 || y2 != 274.0 {
		t.Errorf("slot 2: got (%v,%v); want (16.0, 274.0)", x2, y2)
	}
}

func TestChildSlotInGrid_VariableSizes(t *testing.T) {
	sizes := []nodeSize{
		{100, 80},  // narrow, short
		{300, 200}, // wide, tall
		{200, 150}, // medium
	}
	// maxColW = 300; cols = 2; rows = 2
	// slot 0: col=0, row=0 → x=16, y=130
	x0, y0 := childSlotInGrid(0, sizes)
	if x0 != 16.0 || y0 != 130.0 {
		t.Errorf("slot 0: got (%v,%v); want (16.0, 130.0)", x0, y0)
	}
	// slot 1: col=1, row=0 → x=16+300+14=330, y=130
	x1, y1 := childSlotInGrid(1, sizes)
	if x1 != 330.0 || y1 != 130.0 {
		t.Errorf("slot 1: got (%v,%v); want (330.0, 130.0)", x1, y1)
	}
	// slot 2: col=0, row=1 → x=16, y=130+200+14=344
	x2, y2 := childSlotInGrid(2, sizes)
	if x2 != 16.0 || y2 != 344.0 {
		t.Errorf("slot 2: got (%v,%v); want (16.0, 344.0)", x2, y2)
	}
}

func TestChildSlotInGrid_SingleChild(t *testing.T) {
	sizes := []nodeSize{{400, 300}}
	x, y := childSlotInGrid(0, sizes)
	// cols = 1 (len < 2), maxColW = 400
	// x = 16 + 0*(400+14) = 16, y = 130
	if x != 16.0 || y != 130.0 {
		t.Errorf("single child: got (%v,%v); want (16.0, 130.0)", x, y)
	}
}

func TestChildSlotInGrid_LastSlot(t *testing.T) {
	sizes := []nodeSize{{200, 100}, {200, 100}, {200, 100}}
	// cols = 2, rows = 2, maxColW = 200
	// slot 2: col=0, row=1 → x=16, y=130+100+14=244
	x, y := childSlotInGrid(2, sizes)
	if x != 16.0 || y != 244.0 {
		t.Errorf("last slot: got (%v,%v); want (16.0, 244.0)", x, y)
	}
}

func TestChildSlotInGrid_OverflowIndex(t *testing.T) {
	sizes := []nodeSize{{200, 100}}
	// Index beyond array bounds — Go handles this without panic
	x, y := childSlotInGrid(5, sizes)
	// col = 5 % 2 = 1, row = 5 / 2 = 2
	// x = 16 + 1*(200+14) = 230, y = 130 + 2*(100+14) = 358
	if x != 230.0 || y != 358.0 {
		t.Errorf("overflow index: got (%v,%v); want (230.0, 358.0)", x, y)
	}
}
