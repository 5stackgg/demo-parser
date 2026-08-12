package geometry

import (
	"math"
	"sort"

	"github.com/golang/geo/r3"
)

// bvhNode is one node of a median-split AABB tree. Internal nodes set
// left/right to child indices; leaves set left = -1 and use start/count to
// reference a contiguous run of m.tris.
type bvhNode struct {
	min   r3.Vector
	max   r3.Vector
	left  int
	right int
	start int
	count int
}

const (
	bvhLeafSize = 4
	bvhMaxDepth = 40
)

// build constructs the BVH, reordering m.tris in place so leaves reference
// contiguous ranges.
func (m *Mesh) build() {
	m.nodes = m.nodes[:0]
	if len(m.tris) == 0 {
		return
	}
	// A median split stops at bvhLeafSize and never emits a leaf smaller than
	// two triangles (a node of five splits into two and three), so the tree
	// holds at most len(tris)-1 nodes and in practice about 2/3 of that.
	// Reserve for the typical case and let append cover the rest.
	m.nodes = make([]bvhNode, 0, len(m.tris)*3/4)
	m.buildNode(0, len(m.tris), 0)
	// The mesh is cached for the process lifetime, so trailing capacity is not
	// slack — it is resident memory that nothing will ever write to. Copy into
	// an exact-fit slice and let the oversized one go.
	if cap(m.nodes)-len(m.nodes) > len(m.nodes)/8 {
		m.nodes = append(make([]bvhNode, 0, len(m.nodes)), m.nodes...)
	}
}

func (m *Mesh) buildNode(start, end, depth int) int {
	idx := len(m.nodes)
	m.nodes = append(m.nodes, bvhNode{}) // reserve slot; filled after children
	mn, mx := triBounds(m.tris[start:end])
	node := bvhNode{min: mn, max: mx, left: -1, start: start, count: end - start}

	if end-start <= bvhLeafSize || depth >= bvhMaxDepth {
		m.nodes[idx] = node
		return idx
	}

	// Split along the widest centroid axis at the median.
	ex, ey, ez := mx.X-mn.X, mx.Y-mn.Y, mx.Z-mn.Z
	axis := 0
	if ey > ex && ey >= ez {
		axis = 1
	} else if ez > ex && ez >= ey {
		axis = 2
	}
	// Sort on the centroid, which needs no stored field: the mean of the three
	// corners collapses to v0 plus a third of the two edges, since
	// (v0 + (v0+e1) + (v0+e2))/3 == v0 + (e1+e2)/3. It is only ever a sort key,
	// so computing it at the stored float32 width is fine. One comparator per
	// axis rather than a switch inside the hot one — this runs O(n log n) times
	// per node, at every level of the tree.
	sub := m.tris[start:end]
	switch axis {
	case 0:
		sort.Slice(sub, func(i, j int) bool {
			return sub[i].v0.X+(sub[i].e1.X+sub[i].e2.X)/3 < sub[j].v0.X+(sub[j].e1.X+sub[j].e2.X)/3
		})
	case 1:
		sort.Slice(sub, func(i, j int) bool {
			return sub[i].v0.Y+(sub[i].e1.Y+sub[i].e2.Y)/3 < sub[j].v0.Y+(sub[j].e1.Y+sub[j].e2.Y)/3
		})
	default:
		sort.Slice(sub, func(i, j int) bool {
			return sub[i].v0.Z+(sub[i].e1.Z+sub[i].e2.Z)/3 < sub[j].v0.Z+(sub[j].e1.Z+sub[j].e2.Z)/3
		})
	}
	mid := start + (end-start)/2
	node.left = m.buildNode(start, mid, depth+1)
	node.right = m.buildNode(mid, end, depth+1)
	node.start = 0
	node.count = 0
	m.nodes[idx] = node
	return idx
}

// triBounds is the AABB over a run of triangles. It runs once per BVH node, so
// it sees every triangle at every level of the tree — hot enough that the
// corners are recovered inline and compared with plain branches rather than
// math.Min/Max, which are real calls (they have to honour NaN) and dominated
// the build when this was written the obvious way.
//
// The corners are recovered at full width, the same way rayTriangle does it, so
// the box can never round inward and clip a triangle it is meant to contain.
func triBounds(ts []triangle) (r3.Vector, r3.Vector) {
	mn := r3.Vector{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}
	mx := r3.Vector{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
	for i := range ts {
		t := &ts[i]
		x0, y0, z0 := float64(t.v0.X), float64(t.v0.Y), float64(t.v0.Z)
		x1, y1, z1 := x0+float64(t.e1.X), y0+float64(t.e1.Y), z0+float64(t.e1.Z)
		x2, y2, z2 := x0+float64(t.e2.X), y0+float64(t.e2.Y), z0+float64(t.e2.Z)

		mn.X, mx.X = lo(mn.X, x0, x1, x2), hi(mx.X, x0, x1, x2)
		mn.Y, mx.Y = lo(mn.Y, y0, y1, y2), hi(mx.Y, y0, y1, y2)
		mn.Z, mx.Z = lo(mn.Z, z0, z1, z2), hi(mx.Z, z0, z1, z2)
	}
	return mn, mx
}

// lo and hi are the running min/max over a triangle's three corners. They exist
// instead of math.Min/Max, and instead of the builtin min/max, because both of
// those carry NaN semantics that cost a branch per call; mesh coordinates are
// never NaN (buildMesh reads them straight out of the .tri) and this is the
// hottest loop in the build.
func lo(acc, a, b, c float64) float64 {
	if a < acc {
		acc = a
	}
	if b < acc {
		acc = b
	}
	if c < acc {
		acc = c
	}
	return acc
}

func hi(acc, a, b, c float64) float64 {
	if a > acc {
		acc = a
	}
	if b > acc {
		acc = b
	}
	if c > acc {
		acc = c
	}
	return acc
}

// slabHit is the ray/AABB overlap test over the parametric range [t0, t1].
func slabHit(mn, mx, orig, inv r3.Vector, t0, t1 float64) bool {
	ax := (mn.X - orig.X) * inv.X
	bx := (mx.X - orig.X) * inv.X
	if ax > bx {
		ax, bx = bx, ax
	}
	if ax > t0 {
		t0 = ax
	}
	if bx < t1 {
		t1 = bx
	}
	if t0 > t1 {
		return false
	}
	ay := (mn.Y - orig.Y) * inv.Y
	by := (mx.Y - orig.Y) * inv.Y
	if ay > by {
		ay, by = by, ay
	}
	if ay > t0 {
		t0 = ay
	}
	if by < t1 {
		t1 = by
	}
	if t0 > t1 {
		return false
	}
	az := (mn.Z - orig.Z) * inv.Z
	bz := (mx.Z - orig.Z) * inv.Z
	if az > bz {
		az, bz = bz, az
	}
	if az > t0 {
		t0 = az
	}
	if bz < t1 {
		t1 = bz
	}
	return t0 <= t1
}

// anyHit returns true as soon as any triangle is hit within (tmin, tmax) —
// the early-exit traversal used for occlusion.
func (m *Mesh) anyHit(orig, dir r3.Vector, tmin, tmax float64) bool {
	inv := r3.Vector{X: safeInv(dir.X), Y: safeInv(dir.Y), Z: safeInv(dir.Z)}
	stack := make([]int, 0, 64)
	stack = append(stack, 0)
	for len(stack) > 0 {
		ni := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := &m.nodes[ni]
		if !slabHit(n.min, n.max, orig, inv, tmin, tmax) {
			continue
		}
		if n.left < 0 { // leaf
			for i := n.start; i < n.start+n.count; i++ {
				if t, ok := rayTriangle(orig, dir, &m.tris[i]); ok && t > tmin && t < tmax {
					return true
				}
			}
			continue
		}
		stack = append(stack, n.left, n.right)
	}
	return false
}

// nearestHit returns the closest triangle distance along a (unit) ray.
func (m *Mesh) nearestHit(orig, dir r3.Vector) (float64, bool) {
	inv := r3.Vector{X: safeInv(dir.X), Y: safeInv(dir.Y), Z: safeInv(dir.Z)}
	best := math.Inf(1)
	found := false
	stack := make([]int, 0, 64)
	stack = append(stack, 0)
	for len(stack) > 0 {
		ni := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := &m.nodes[ni]
		if !slabHit(n.min, n.max, orig, inv, 1e-4, best) {
			continue
		}
		if n.left < 0 {
			for i := n.start; i < n.start+n.count; i++ {
				if t, ok := rayTriangle(orig, dir, &m.tris[i]); ok && t > 1e-4 && t < best {
					best = t
					found = true
				}
			}
			continue
		}
		stack = append(stack, n.left, n.right)
	}
	return best, found
}
