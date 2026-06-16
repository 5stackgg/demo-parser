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
	m.nodes = make([]bvhNode, 0, 2*len(m.tris))
	m.buildNode(0, len(m.tris), 0)
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
	sub := m.tris[start:end]
	sort.Slice(sub, func(i, j int) bool {
		switch axis {
		case 0:
			return sub[i].cx < sub[j].cx
		case 1:
			return sub[i].cy < sub[j].cy
		default:
			return sub[i].cz < sub[j].cz
		}
	})
	mid := start + (end-start)/2
	node.left = m.buildNode(start, mid, depth+1)
	node.right = m.buildNode(mid, end, depth+1)
	node.start = 0
	node.count = 0
	m.nodes[idx] = node
	return idx
}

func triBounds(ts []triangle) (r3.Vector, r3.Vector) {
	mn := r3.Vector{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}
	mx := r3.Vector{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
	for i := range ts {
		t := &ts[i]
		mn.X, mn.Y, mn.Z = math.Min(mn.X, t.min.X), math.Min(mn.Y, t.min.Y), math.Min(mn.Z, t.min.Z)
		mx.X, mx.Y, mx.Z = math.Max(mx.X, t.max.X), math.Max(mx.Y, t.max.Y), math.Max(mx.Z, t.max.Z)
	}
	return mn, mx
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
