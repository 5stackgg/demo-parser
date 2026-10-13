package parser

import (
	"testing"

	"github.com/golang/geo/r3"
)

var recordedVoxelLogs = []string{
	"smoke_voxels_inferno_4019.bin",
	"smoke_voxels_inferno_12642.bin",
	"smoke_voxels_inferno_14690.bin",
}

// The server sends a brick the first time its simulation reads geometry out
// of it, so a recorded log says, step by step, how far the game's own cloud
// had spread. A replay that drifts from the game by anything that matters
// wakes a voxel a step early or late and reads a brick in the wrong frame.
//
// The first frame also carries the bricks the server read while it placed the
// emitters, which the replay may get to later or never.
func TestSimulationReadsTheBricksTheServerSent(t *testing.T) {
	for _, file := range recordedVoxelLogs {
		t.Run(file, func(t *testing.T) {
			frames, _, err := decodeVoxelFrames(readVoxelDump(t, file))
			if err != nil {
				t.Fatal(err)
			}
			l := newVoxelLog()
			sentIn := map[int]int{}
			for _, f := range frames {
				l.apply(f, 0)
				for _, b := range f.Bricks {
					brick := int(b.Index) & (voxelBricks - 1)
					if _, ok := sentIn[brick]; !ok {
						sentIn[brick] = f.Number
					}
				}
			}
			sim := l.simulation(r3.Vector{})
			read := map[int]bool{}
			var fresh map[int]bool
			sim.touched = func(brick int) {
				if !read[brick] {
					fresh[brick] = true
				}
			}
			matched := 0
			for n := range frames {
				fresh = map[int]bool{}
				if !l.advance(sim, n) {
					t.Fatalf("step %d: the smoke is fading", n)
				}
				for brick := range fresh {
					read[brick] = true
					in, sent := sentIn[brick]
					if !sent {
						t.Fatalf("step %d read brick %d, which the server never sent", n, brick)
					}
					if in != n && in != 0 {
						t.Fatalf("step %d read brick %d, which the server sent in frame %d", n, brick, in)
					}
				}
				if n == 0 {
					continue
				}
				for brick, in := range sentIn {
					if in != n {
						continue
					}
					if !fresh[brick] {
						t.Fatalf("the server sent brick %d in frame %d; the replay did not read it then", brick, n)
					}
					matched++
				}
			}
			if matched < 15 {
				t.Fatalf("only %d bricks arrived after the first frame; the log proves little", matched)
			}
			t.Logf("%d steps, %d bricks read in the frame the server sent them", len(frames), matched)
		})
	}
}

// openSim is a simulation with nothing in the grid but its emitters.
func openSim(emitters ...simEmitter) *smokeSim {
	sim := newSmokeSim([3]float32{})
	sim.emitters = emitters
	return sim
}

func (s *smokeSim) densityAt(x, y, z int) float32 {
	return s.cell[s.cur][interleave3(x, y, z)].density
}

func (s *smokeSim) wall(inside func(x, y, z int) bool) {
	for m := range s.solid {
		if inside(deinterleave3(m, 5)) {
			s.solid[m] = true
		}
	}
}

// inSmoke are the voxels holding enough to be part of the cloud.
func (s *smokeSim) inSmoke() (cells [][3]int) {
	for m := range s.cell[s.cur] {
		if s.cell[s.cur][m].density > smokeVisible {
			x, y, z := deinterleave3(m, 5)
			cells = append(cells, [3]int{x, y, z})
		}
	}
	return cells
}

func TestTheFirstStepIsABurstAroundEveryEmitter(t *testing.T) {
	// The second emitter is far past the 80 units inside which one emits
	// from the start.
	sim := openSim(simEmitter{x: 16, y: 16, z: 16}, simEmitter{x: 26, y: 16, z: 16})
	sim.step()
	for _, e := range sim.emitters {
		for _, d := range simAround {
			if got := sim.densityAt(e.x+d[0], e.y+d[1], e.z+d[2]); got < simEmitRate*simBurstShare/2 {
				t.Fatalf("the voxel %v from the emitter at %d holds %v after the burst", d, e.x, got)
			}
		}
	}
	// The emitters' own voxels fill from the ones around them in the same step.
	if n, want := len(sim.inSmoke()), 2*(len(simAround)+1); n != want {
		t.Fatalf("the burst filled %d voxels, want the %d at and around the two emitters", n, want)
	}
	sim.step()
	for _, e := range sim.emitters {
		if got := sim.densityAt(e.x, e.y, e.z); got < simFullDensity/2 {
			t.Fatalf("the emitter at %d holds %v a step after the burst", e.x, got)
		}
	}
}

// A voxel takes more from the one above it than from the one below, so a
// cloud with nothing under it hangs lower than its emitter.
func TestSmokeSinks(t *testing.T) {
	sim := openSim(simEmitter{x: 16, y: 16, z: 16})
	for n := 0; n < 60; n++ {
		sim.step()
	}
	for _, off := range []int{1, 2} {
		below, above := sim.densityAt(16, 16, 16-off), sim.densityAt(16, 16, 16+off)
		if below <= above {
			t.Fatalf("%d below the emitter holds %v and %d above it %v", off, below, off, above)
		}
	}
}

func TestSmokeStopsAtAWall(t *testing.T) {
	sim := openSim(simEmitter{x: 16, y: 16, z: 16}, simEmitter{x: 16, y: 18, z: 16}, simEmitter{x: 16, y: 14, z: 16})
	sim.wall(func(x, y, z int) bool { return x == 18 })
	for n := 0; n < 60; n++ {
		sim.step()
	}
	against := 0
	for _, c := range sim.inSmoke() {
		if c[0] == 17 {
			against++
		}
	}
	if against < 9 {
		t.Fatalf("%d voxels of the cloud are against the wall; it should have spread along it", against)
	}
	for m, c := range sim.cell[sim.cur] {
		if x, y, z := deinterleave3(m, 5); x >= 18 && c.density != 0 {
			t.Fatalf("voxel (%d,%d,%d), at or past the wall, holds %v", x, y, z, c.density)
		}
	}
}

// When a smoke goes off on a ledge the server puts emitters down the drop
// beside it, and those are what carry the cloud to the floor below: one
// unbroken cloud from the ledge to the ground.
func TestEmittersDownADropCarryTheCloudToTheFloor(t *testing.T) {
	ledge := []simEmitter{{x: 14, y: 16, z: 16}, {x: 12, y: 16, z: 16}, {x: 14, y: 14, z: 16}, {x: 14, y: 18, z: 16}}
	drop := []simEmitter{{x: 17, y: 16, z: 14}, {x: 17, y: 16, z: 11}, {x: 17, y: 16, z: 9}, {x: 17, y: 16, z: 7}}
	settle := func(emitters []simEmitter) *smokeSim {
		sim := openSim(emitters...)
		sim.wall(func(x, y, z int) bool { return (z <= 15 && z >= 5 && x <= 15) || z == 4 })
		for n := 0; n < 100; n++ {
			sim.step()
		}
		return sim
	}
	sim := settle(append(append([]simEmitter{}, ledge...), drop...))
	for z := 5; z <= 16; z++ {
		if got := sim.densityAt(17, 16, z); got <= smokeVisible {
			t.Fatalf("the drop holds %v at height %d; the cloud should be unbroken down to the floor", got, z)
		}
	}
	// Without them the cloud stays up on the ledge.
	if got := settle(ledge).densityAt(17, 16, 5); got > smokeVisible {
		t.Fatalf("with no emitter down the drop, the floor still holds %v", got)
	}
}

// What a voxel loses each step bounds the cloud: it stops growing.
func TestACloudSettles(t *testing.T) {
	var emitters []simEmitter
	for y := 12; y <= 20; y += 2 {
		for x := 12; x <= 20; x += 2 {
			emitters = append(emitters, simEmitter{x: x, y: y, z: 16})
		}
	}
	sim := openSim(emitters...)
	sim.wall(func(x, y, z int) bool { return z == 15 })
	sizes := map[int]int{}
	for n := 1; n <= 100; n++ {
		sim.step()
		sizes[n] = len(sim.inSmoke())
	}
	if sizes[100] < 300 || sizes[100] > 2000 {
		t.Fatalf("the settled cloud holds %d voxels", sizes[100])
	}
	if grew := float64(sizes[100]-sizes[80]) / float64(sizes[80]); grew > 0.03 || grew < -0.03 {
		t.Fatalf("the cloud went from %d to %d voxels over its last two seconds", sizes[80], sizes[100])
	}
	for _, c := range sim.inSmoke() {
		if c[2] < 16 {
			t.Fatalf("voxel %v is under the floor", c)
		}
	}
}

func TestTheSimulationIsDeterministic(t *testing.T) {
	run := func() [voxelCells]simCell {
		sim := openSim(simEmitter{x: 14, y: 16, z: 16}, simEmitter{x: 18, y: 17, z: 18})
		sim.wall(func(x, y, z int) bool { return z == 15 || (y == 19 && z < 20) })
		for n := 0; n < 40; n++ {
			sim.step()
		}
		return sim.cell[sim.cur]
	}
	if run() != run() {
		t.Fatal("two runs from the same start ended differently")
	}
}

// The table must spread bits exactly as the game's loop does, for every voxel.
func TestInterleave3RoundTrips(t *testing.T) {
	for m := 0; m < voxelCells; m++ {
		x, y, z := deinterleave3(m, 5)
		if got := interleave3(x, y, z); got != m {
			t.Fatalf("interleave3(%d, %d, %d) = %d, want %d", x, y, z, got, m)
		}
	}
}
