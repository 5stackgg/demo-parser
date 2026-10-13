package geometry

import (
	"math"
	"slices"

	"github.com/golang/geo/r3"
)

// bvhNode is one node of a flattened AABB tree, laid out depth first: an
// internal node's left child is the node straight after it and index names its
// right child; a leaf's index is its first triangle in m.tris.
//
// The box is float32 and still exact. Every corner the tree bounds came off
// the .tri as a float32, and a min or max of float32s is one of them, so
// nothing rounds. The slab test widens it back to float64 and so culls exactly
// what a float64 box would. 32 bytes, so two share a cache line — the old
// float64 node was 80 and a ray's walk down the tree was mostly cache misses.
type bvhNode struct {
	min, max [3]float32
	index    uint32
	// meta is count<<2 | axis. count is the leaf's triangle count and 0 for an
	// internal node, whose split axis picks which child a ray visits first.
	meta uint32
}

func (n *bvhNode) leaf() bool { return n.meta>>2 != 0 }
func (n *bvhNode) count() int { return int(n.meta >> 2) }
func (n *bvhNode) axis() int  { return int(n.meta & 3) }

const (
	// bvhLeafSize is the run that is always a leaf. Above it the surface area
	// heuristic decides, up to bvhMaxLeaf, past which a node is always split.
	bvhLeafSize = 4
	bvhMaxLeaf  = 16
	// bvhBins is how many candidate planes per axis the build prices.
	bvhBins = 16
	// bvhMaxDepth bounds the tree, and with it the fixed traversal stack.
	bvhMaxDepth = 64
)

// buildRef is one triangle while the tree is being built: its bounds and the
// centroid it is binned by. Nothing here outlives the build.
type buildRef struct {
	lo, hi [3]float32
	// c is the sum of the corners: a third of it is the centroid, and only
	// the order matters.
	c   [3]float32
	tri uint32
}

// build constructs the BVH, reordering m.tris in place so leaves reference
// contiguous ranges.
//
// The split is a binned surface area heuristic: each axis is cut into
// bvhBins slabs by centroid and the plane that minimises the expected cost of
// a ray through the two children wins. A median split halves the triangle
// count but not the space, so on a map — long open sightlines over dense
// clusters of props — it leaves boxes that every ray has to enter. Pricing
// splits by area is what makes a ray across open ground touch few of them. It
// is also cheaper to build: binning is linear per level where the median
// needed a sort.
//
// The build is deterministic: the same .tri always gives the same tree, and
// therefore the same traversal order and the same tie-breaks in
// nearestHitTri.
func (m *Mesh) build() {
	if len(m.tris) == 0 {
		m.nodes = nil
		return
	}
	refs := make([]buildRef, len(m.tris))
	for i := range m.tris {
		t := &m.tris[i]
		r := &refs[i]
		r.tri = uint32(i)
		for a := 0; a < 3; a++ {
			v0, v1, v2 := t.v0.axis(a), t.v1.axis(a), t.v2.axis(a)
			r.lo[a] = min(v0, v1, v2)
			r.hi[a] = max(v0, v1, v2)
			r.c[a] = v0 + v1 + v2
		}
	}
	m.nodes = make([]bvhNode, 0, len(m.tris)*4/5+1)
	m.buildNode(refs, 0, 0)

	// Put the triangles in leaf order in place, following each cycle of the
	// permutation, rather than holding a second copy of the mesh.
	const placed = math.MaxUint32
	for i := range refs {
		if refs[i].tri == placed {
			continue
		}
		tmp := m.tris[i]
		j := i
		for {
			k := int(refs[j].tri)
			refs[j].tri = placed
			if k == i {
				m.tris[j] = tmp
				break
			}
			m.tris[j] = m.tris[k]
			j = k
		}
	}

	// The mesh is then cached for the process lifetime, so what is left over is
	// not slack — it is resident memory nothing will ever write to.
	if cap(m.nodes) > len(m.nodes) {
		m.nodes = slices.Clone(m.nodes)
	}
}

func (v vec3) axis(a int) float32 {
	switch a {
	case 0:
		return v.X
	case 1:
		return v.Y
	}
	return v.Z
}

// buildNode appends the subtree over refs, whose triangles will sit at
// m.tris[start:start+len(refs)].
func (m *Mesh) buildNode(refs []buildRef, start, depth int) {
	idx := len(m.nodes)
	m.nodes = append(m.nodes, bvhNode{})

	lo, hi := refs[0].lo, refs[0].hi
	clo, chi := refs[0].c, refs[0].c
	for i := 1; i < len(refs); i++ {
		r := &refs[i]
		for a := 0; a < 3; a++ {
			lo[a] = min(lo[a], r.lo[a])
			hi[a] = max(hi[a], r.hi[a])
			clo[a] = min(clo[a], r.c[a])
			chi[a] = max(chi[a], r.c[a])
		}
	}
	node := bvhNode{min: lo, max: hi}
	n := len(refs)
	makeLeaf := func() {
		node.index = uint32(start)
		node.meta = uint32(n) << 2
		m.nodes[idx] = node
	}
	if n <= bvhLeafSize || depth >= bvhMaxDepth {
		makeLeaf()
		return
	}

	axis, plane, cost := sahSplit(refs, lo, hi, clo, chi)
	if plane < 0 || (cost >= float64(n) && n <= bvhMaxLeaf) {
		if plane < 0 && n > bvhMaxLeaf {
			// Every centroid is in one bin: there is no plane to price. Halve
			// the run as it stands, which still bounds the leaf.
			axis = 0
		} else {
			makeLeaf()
			return
		}
	}

	mid := n / 2
	if plane >= 0 {
		// Partition in place: refs left of the plane to the front.
		k := binScale(clo[axis], chi[axis])
		i, j := 0, n-1
		for i <= j {
			if binOf(refs[i].c[axis], clo[axis], k) < plane {
				i++
				continue
			}
			refs[i], refs[j] = refs[j], refs[i]
			j--
		}
		mid = i
	}

	m.buildNode(refs[:mid], start, depth+1)
	node.index = uint32(len(m.nodes))
	node.meta = uint32(axis)
	m.buildNode(refs[mid:], start+mid, depth+1)
	m.nodes[idx] = node
}

func binScale(lo, hi float32) float64 {
	return bvhBins / (float64(hi) - float64(lo))
}

func binOf(c, lo float32, k float64) int {
	b := int((float64(c) - float64(lo)) * k)
	if b >= bvhBins {
		b = bvhBins - 1
	}
	return b
}

func area(lo, hi [3]float32) float64 {
	dx := float64(hi[0]) - float64(lo[0])
	dy := float64(hi[1]) - float64(lo[1])
	dz := float64(hi[2]) - float64(lo[2])
	return dx*dy + dy*dz + dz*dx
}

// sahSplit prices every bin boundary on every axis and returns the cheapest:
// the axis, the first bin on the right of the plane, and the expected cost of
// splitting there in triangle tests (a node visit counts as one). plane is -1
// when no boundary separates anything.
func sahSplit(refs []buildRef, lo, hi, clo, chi [3]float32) (axis, plane int, cost float64) {
	parent := area(lo, hi)
	plane = -1
	cost = math.Inf(1)
	if parent <= 0 {
		return
	}
	type bin struct {
		lo, hi [3]float32
		n      int
	}
	for a := 0; a < 3; a++ {
		if !(chi[a] > clo[a]) {
			continue
		}
		var bins [bvhBins]bin
		k := binScale(clo[a], chi[a])
		for i := range refs {
			r := &refs[i]
			b := &bins[binOf(r.c[a], clo[a], k)]
			if b.n == 0 {
				b.lo, b.hi = r.lo, r.hi
			} else {
				for d := 0; d < 3; d++ {
					b.lo[d] = min(b.lo[d], r.lo[d])
					b.hi[d] = max(b.hi[d], r.hi[d])
				}
			}
			b.n++
		}
		// rightCost[p] is area × count of everything in bins p and up.
		var rightCost [bvhBins]float64
		var rlo, rhi [3]float32
		rn := 0
		for p := bvhBins - 1; p > 0; p-- {
			b := &bins[p]
			if b.n > 0 {
				if rn == 0 {
					rlo, rhi = b.lo, b.hi
				} else {
					for d := 0; d < 3; d++ {
						rlo[d] = min(rlo[d], b.lo[d])
						rhi[d] = max(rhi[d], b.hi[d])
					}
				}
				rn += b.n
			}
			if rn > 0 {
				rightCost[p] = area(rlo, rhi) * float64(rn)
			}
		}
		var llo, lhi [3]float32
		ln := 0
		for p := 1; p < bvhBins; p++ {
			b := &bins[p-1]
			if b.n > 0 {
				if ln == 0 {
					llo, lhi = b.lo, b.hi
				} else {
					for d := 0; d < 3; d++ {
						llo[d] = min(llo[d], b.lo[d])
						lhi[d] = max(lhi[d], b.hi[d])
					}
				}
				ln += b.n
			}
			if ln == 0 || ln == len(refs) {
				continue
			}
			c := 1 + (area(llo, lhi)*float64(ln)+rightCost[p])/parent
			if c < cost {
				axis, plane, cost = a, p, c
			}
		}
	}
	return
}

// slabHit is the ray/AABB overlap test over the parametric range [t0, t1].
func slabHit(n *bvhNode, orig, inv r3.Vector, t0, t1 float64) bool {
	ax := (float64(n.min[0]) - orig.X) * inv.X
	bx := (float64(n.max[0]) - orig.X) * inv.X
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
	ay := (float64(n.min[1]) - orig.Y) * inv.Y
	by := (float64(n.max[1]) - orig.Y) * inv.Y
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
	az := (float64(n.min[2]) - orig.Z) * inv.Z
	bz := (float64(n.max[2]) - orig.Z) * inv.Z
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

// children returns an internal node's children, the one nearer the ray's
// origin along the split axis first.
func children(ni uint32, n *bvhNode, neg *[3]bool) (near, far uint32) {
	if neg[n.axis()] {
		return n.index, ni + 1
	}
	return ni + 1, n.index
}

// anyHit returns true as soon as any triangle is hit within (tmin, tmax) —
// the early-exit traversal used for occlusion. It walks near child first like
// nearestHitTri; here any hit ends the walk, so the order matters little.
func (m *Mesh) anyHit(orig, dir r3.Vector, tmin, tmax float64) bool {
	inv := r3.Vector{X: safeInv(dir.X), Y: safeInv(dir.Y), Z: safeInv(dir.Z)}
	neg := [3]bool{dir.X < 0, dir.Y < 0, dir.Z < 0}
	var stack [bvhMaxDepth]uint32
	sp := 0
	ni := uint32(0)
	for {
		n := &m.nodes[ni]
		if slabHit(n, orig, inv, tmin, tmax) {
			if !n.leaf() {
				near, far := children(ni, n, &neg)
				stack[sp] = far
				sp++
				ni = near
				continue
			}
			for i, end := int(n.index), int(n.index)+n.count(); i < end; i++ {
				if t, ok := rayTriangle(orig, dir, &m.tris[i]); ok && t > tmin && t < tmax {
					return true
				}
			}
		}
		if sp == 0 {
			return false
		}
		sp--
		ni = stack[sp]
	}
}

// nearestHit returns the closest triangle distance along a (unit) ray.
func (m *Mesh) nearestHit(orig, dir r3.Vector) (float64, bool) {
	t, _, ok := m.nearestHitTri(orig, dir)
	return t, ok
}

// nearestHitTri is nearestHit plus the triangle that produced the hit, for
// callers that need the surface (its normal) and not just the range to it.
//
// Near child first, so the first hit found is usually the answer and the
// far side of the tree is culled against it rather than searched.
//
// Ties are broken by traversal order rather than arbitrarily: `<` keeps the
// first triangle found at a given distance, and the traversal itself is a
// fixed stack walk over a deterministically built tree. Two processes loading
// the same .tri therefore pick the same triangle, which is what lets a
// simulation run against two meshes be compared at all.
func (m *Mesh) nearestHitTri(orig, dir r3.Vector) (float64, *triangle, bool) {
	inv := r3.Vector{X: safeInv(dir.X), Y: safeInv(dir.Y), Z: safeInv(dir.Z)}
	neg := [3]bool{dir.X < 0, dir.Y < 0, dir.Z < 0}
	best := math.Inf(1)
	var hit *triangle
	var stack [bvhMaxDepth]uint32
	sp := 0
	ni := uint32(0)
	for {
		n := &m.nodes[ni]
		if slabHit(n, orig, inv, 1e-4, best) {
			if !n.leaf() {
				near, far := children(ni, n, &neg)
				stack[sp] = far
				sp++
				ni = near
				continue
			}
			for i, end := int(n.index), int(n.index)+n.count(); i < end; i++ {
				if t, ok := rayTriangle(orig, dir, &m.tris[i]); ok && t > 1e-4 && t < best {
					best = t
					hit = &m.tris[i]
				}
			}
		}
		if sp == 0 {
			return best, hit, hit != nil
		}
		sp--
		ni = stack[sp]
	}
}
