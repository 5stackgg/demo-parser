package parser

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math/bits"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/geo/r3"
)

// voxelFrameBytes encodes one frame of the networked log.
func voxelFrameBytes(n int, sources []voxelSource, bricks []voxelBrick) []byte {
	var p []byte
	flags := byte(0)
	if len(sources) > 0 {
		flags |= 1
	}
	if len(bricks) > 0 {
		flags |= 2
	}
	p = append(p, 0, flags)
	if len(sources) > 0 {
		p = append(p, byte(len(sources)))
		for _, s := range sources {
			p = append(p, s.X, s.Y, s.Z, s.V, 0, 0, 0, 0)
		}
	}
	if len(bricks) > 0 {
		p = binary.LittleEndian.AppendUint16(p, uint16(len(bricks)))
		for _, b := range bricks {
			p = binary.LittleEndian.AppendUint16(p, b.Index)
			p = binary.LittleEndian.AppendUint64(p, b.Mask)
		}
	}
	p = append(p, 0)
	out := binary.LittleEndian.AppendUint16(nil, uint16(n))
	out = binary.LittleEndian.AppendUint16(out, uint16(len(p)))
	return append(out, p...)
}

func TestBrickCoordIsMorton(t *testing.T) {
	cases := []struct {
		index   uint16
		x, y, z int
	}{
		{0, 0, 0, 0},
		{1, 1, 0, 0},
		{2, 0, 1, 0},
		{4, 0, 0, 1},
		{8, 2, 0, 0},
		{64, 4, 0, 0},
		{448, 4, 4, 4},
		{511, 7, 7, 7},
	}
	for _, c := range cases {
		x, y, z := brickCoord(c.index)
		if x != c.x || y != c.y || z != c.z {
			t.Errorf("brick %d = (%d,%d,%d), want (%d,%d,%d)", c.index, x, y, z, c.x, c.y, c.z)
		}
	}
	seen := map[[3]int]bool{}
	for i := 0; i < voxelBricks; i++ {
		x, y, z := brickCoord(uint16(i))
		seen[[3]int{x, y, z}] = true
	}
	if len(seen) != voxelBricks {
		t.Fatalf("brick indices cover %d distinct bricks, want %d", len(seen), voxelBricks)
	}
}

func TestMaskBitIsMorton(t *testing.T) {
	cases := []struct {
		bit     int
		x, y, z int
	}{
		{0, 0, 0, 0},
		{1, 1, 0, 0},
		{2, 0, 1, 0},
		{4, 0, 0, 1},
		{8, 2, 0, 0},
		{16, 0, 2, 0},
		{32, 0, 0, 2},
		{63, 3, 3, 3},
	}
	for _, c := range cases {
		x, y, z := maskVoxel(c.bit)
		if x != c.x || y != c.y || z != c.z {
			t.Errorf("bit %d = (%d,%d,%d), want (%d,%d,%d)", c.bit, x, y, z, c.x, c.y, c.z)
		}
	}
}

func TestDecodeVoxelFrames(t *testing.T) {
	src := []voxelSource{{X: 16, Y: 16, Z: 15, V: 5}, {X: 17, Y: 16, Z: 15, V: 0}}
	buf := voxelFrameBytes(0, src, []voxelBrick{{Index: 7, Mask: 1 << 63}})
	buf = append(buf, voxelFrameBytes(1, nil, nil)...)
	buf = append(buf, voxelFrameBytes(2, nil, []voxelBrick{{Index: 511, Mask: 0}, {Index: 448, Mask: 3}})...)
	whole := len(buf)
	// A frame the server is still writing: the header promises more than has
	// arrived.
	partial := voxelFrameBytes(3, nil, []voxelBrick{{Index: 1, Mask: 1}})
	buf = append(buf, partial[:len(partial)-4]...)

	frames, next, err := decodeVoxelFrames(buf)
	if err != nil {
		t.Fatal(err)
	}
	if next != whole {
		t.Fatalf("next = %d, want %d (the end of the last complete frame)", next, whole)
	}
	if len(frames) != 3 {
		t.Fatalf("decoded %d frames, want 3", len(frames))
	}
	if f := frames[0]; f.Number != 0 || len(f.Sources) != 2 || f.Sources[1] != src[1] || len(f.Bricks) != 1 || f.Bricks[0].Mask != 1<<63 {
		t.Errorf("frame 0 = %+v", f)
	}
	if f := frames[1]; f.Number != 1 || len(f.Sources) != 0 || len(f.Bricks) != 0 {
		t.Errorf("frame 1 = %+v", f)
	}
	if f := frames[2]; f.Number != 2 || len(f.Bricks) != 2 || f.Bricks[1] != (voxelBrick{Index: 448, Mask: 3}) {
		t.Errorf("frame 2 = %+v", f)
	}

	// Brick 7 is (1,1,1); mask bit 63 is its far corner, voxel (7,7,7).
	l := newVoxelLog()
	for i, f := range frames {
		l.apply(f, i*7)
	}
	if !l.solid[voxelIndex(7, 7, 7)] || l.solid[voxelIndex(4, 4, 4)] {
		t.Error("brick 7's mask should mark exactly its (3,3,3) voxel")
	}
	// Brick 448 is (4,4,4), voxels 16-19 on each axis.
	if l.brickTick[brickOf(16, 16, 16)] != 14 {
		t.Errorf("brick 448 arrived at %d, want 14", l.brickTick[brickOf(16, 16, 16)])
	}
	if !l.solid[voxelIndex(16, 16, 16)] || !l.solid[voxelIndex(17, 16, 16)] || l.solid[voxelIndex(16, 17, 16)] {
		t.Error("brick 448's mask 0b11 should mark its first two voxels along x")
	}
}

func TestDecodeVoxelFramesRejectsOverrun(t *testing.T) {
	frame := voxelFrameBytes(0, nil, []voxelBrick{{Index: 1, Mask: 1}})
	// Claim three bricks in a payload sized for one.
	frame[4+2] = 3
	if _, _, err := decodeVoxelFrames(frame); err == nil {
		t.Fatal("a brick count past the payload should be an error")
	}
}

func readVoxelDump(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Logs captured off test.dem's three de_inferno smokes.
func TestDecodeRecordedVoxelLogs(t *testing.T) {
	cases := []struct {
		file          string
		frames        int
		frame0Bricks  int
		totalBricks   int
		firstSource   voxelSource
		firstBrick    voxelBrick
		firstBrickSet int
	}{
		{"smoke_voxels_inferno_4019.bin", 52, 30, 52, voxelSource{14, 18, 16, 5}, voxelBrick{Index: 448}, 0},
		{"smoke_voxels_inferno_12642.bin", 40, 37, 68, voxelSource{}, voxelBrick{Index: 116}, 32},
		{"smoke_voxels_inferno_14690.bin", 37, 36, 76, voxelSource{}, voxelBrick{Index: 286}, 28},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			buf := readVoxelDump(t, c.file)
			frames, next, err := decodeVoxelFrames(buf)
			if err != nil {
				t.Fatal(err)
			}
			if next != len(buf) {
				t.Fatalf("decoded %d of %d bytes", next, len(buf))
			}
			if len(frames) != c.frames {
				t.Fatalf("%d frames, want %d", len(frames), c.frames)
			}
			total := 0
			for i, f := range frames {
				if f.Number != i {
					t.Fatalf("frame %d numbered %d; the log should be contiguous", i, f.Number)
				}
				if i > 0 && len(f.Sources) > 0 {
					t.Errorf("frame %d carries sources; only the first should", i)
				}
				total += len(f.Bricks)
			}
			f0 := frames[0]
			if len(f0.Sources) != 44 || len(f0.Bricks) != c.frame0Bricks {
				t.Fatalf("frame 0: %d sources, %d bricks", len(f0.Sources), len(f0.Bricks))
			}
			if c.firstSource != (voxelSource{}) && f0.Sources[0] != c.firstSource {
				t.Errorf("first source %+v, want %+v", f0.Sources[0], c.firstSource)
			}
			if f0.Bricks[0].Index != c.firstBrick.Index || bits.OnesCount64(f0.Bricks[0].Mask) != c.firstBrickSet {
				t.Errorf("first brick %d with %d solid, want %d with %d",
					f0.Bricks[0].Index, bits.OnesCount64(f0.Bricks[0].Mask), c.firstBrick.Index, c.firstBrickSet)
			}
			if total != c.totalBricks {
				t.Errorf("%d bricks in all, want %d", total, c.totalBricks)
			}
			for _, s := range f0.Sources {
				if s.X >= voxelGridDim || s.Y >= voxelGridDim || s.Z >= voxelGridDim {
					t.Fatalf("source %+v outside the grid", s)
				}
			}
		})
	}
}

// The server appends to the log a few bytes at a time; reading it in pieces
// must yield the same frames as reading it whole.
func TestDecodeVoxelLogIncrementally(t *testing.T) {
	buf := readVoxelDump(t, "smoke_voxels_inferno_4019.bin")
	want, _, _ := decodeVoxelFrames(buf)
	for _, step := range []int{1, 7, 64, 500} {
		var got []voxelFrame
		next := 0
		for end := step; ; end += step {
			if end > len(buf) {
				end = len(buf)
			}
			frames, n, err := decodeVoxelFrames(buf[next:end])
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, frames...)
			next += n
			if end == len(buf) {
				break
			}
		}
		if len(got) != len(want) || next != len(buf) {
			t.Fatalf("step %d: %d frames ending at %d, want %d ending at %d", step, len(got), next, len(want), len(buf))
		}
	}
}

// openLog is a log where every brick arrived on the detonation frame and the
// world is empty, so only the sources and the budget shape the cloud.
func openLog(sources ...voxelSource) *voxelLog {
	l := newVoxelLog()
	for i := range l.brickTick {
		l.brickTick[i] = 0
	}
	l.sources = sources
	return l
}

func filledCells(arrival []int) int {
	n := 0
	for _, t := range arrival {
		if t >= 0 {
			n++
		}
	}
	return n
}

func TestFloodStopsAtAWall(t *testing.T) {
	l := openLog(voxelSource{X: 12, Y: 16, Z: 16, V: 5})
	for z := 0; z < voxelGridDim; z++ {
		for y := 0; y < voxelGridDim; y++ {
			l.solid[voxelIndex(16, y, z)] = true
		}
	}
	arrival := l.flood(defaultVoxelFlood, 64)
	if n := filledCells(arrival); n < defaultVoxelFlood.budgetMin {
		t.Fatalf("cloud holds %d voxels, want at least %d", n, defaultVoxelFlood.budgetMin)
	}
	for i, a := range arrival {
		if a < 0 {
			continue
		}
		if x, _, _ := voxelCoord(i); x >= 16 {
			t.Fatalf("voxel at x=%d filled through the wall", x)
		}
	}
}

func TestFloodNeverEntersAnUnsentBrick(t *testing.T) {
	l := openLog(voxelSource{X: 16, Y: 16, Z: 16, V: 5})
	unsent := brickOf(20, 16, 16)
	l.brickTick[unsent] = -1
	for i, a := range l.flood(defaultVoxelFlood, 64) {
		if a >= 0 && brickOf(voxelCoord(i)) == unsent {
			t.Fatal("the cloud filled a brick the server never sent")
		}
	}
}

func TestFloodTiming(t *testing.T) {
	l := openLog(voxelSource{X: 16, Y: 16, Z: 16, V: 5})
	late := brickOf(24, 16, 16)
	l.brickTick[late] = 200
	p := defaultVoxelFlood
	arrival := l.flood(p, 64)
	step := int(p.stepSecs*64 + 0.5)

	if got := arrival[voxelIndex(16, 16, 16)]; got != 0 {
		t.Errorf("the source filled at %d, want 0", got)
	}
	if got := arrival[voxelIndex(17, 16, 16)]; got != step {
		t.Errorf("its neighbour filled at %d, want one step (%d)", got, step)
	}
	if got := arrival[voxelIndex(19, 16, 16)]; got != 3*step {
		t.Errorf("three voxels out filled at %d, want %d", got, 3*step)
	}
	for i, a := range arrival {
		if a >= 0 && brickOf(voxelCoord(i)) == late && a < 200 {
			t.Fatalf("a voxel filled at %d, before its brick arrived at 200", a)
		}
	}
}

// The budget fit should settle on the cloud whose shell matches the bricks
// the server sent — here, exactly the bricks a known-size cloud touches.
func TestFitBudgetRecoversTheCloudSize(t *testing.T) {
	l := openLog(voxelSource{X: 16, Y: 16, Z: 16, V: 5})
	order := l.fillOrder(defaultVoxelFlood.budgetMax)
	const size = 900
	var sent [voxelBricks]bool
	for _, c := range order[:size] {
		x, y, z := voxelCoord(c)
		for _, d := range [7][3]int{{}, {1, 0, 0}, {-1, 0, 0}, {0, 1, 0}, {0, -1, 0}, {0, 0, 1}, {0, 0, -1}} {
			sent[brickOf(x+d[0], y+d[1], z+d[2])] = true
		}
	}
	for b := range l.brickTick {
		if !sent[b] {
			l.brickTick[b] = -1
		}
	}
	order = l.fillOrder(defaultVoxelFlood.budgetMax)
	budget, iou := l.fitBudget(order, defaultVoxelFlood)
	if iou != 1 {
		t.Fatalf("best fit scores %.3f, want a perfect match", iou)
	}
	if got := l.brickIoU(order[:budget]); got != 1 {
		t.Fatalf("the fitted cloud scores %.3f on its own", got)
	}
}

func TestExportVoxelsPacksXFastest(t *testing.T) {
	arrival := make([]int, voxelCells)
	for i := range arrival {
		arrival[i] = -1
	}
	arrival[voxelIndex(10, 20, 5)] = 0
	arrival[voxelIndex(12, 20, 5)] = 64     // 1 s
	arrival[voxelIndex(10, 21, 6)] = 64 * 9 // past the byte's range
	det := r3.Vector{X: 1000, Y: -500, Z: 64}

	out, ok := exportVoxels(arrival, det, 64)
	if !ok {
		t.Fatal("expected a cloud")
	}
	if out.DimX != 3 || out.DimY != 2 || out.DimZ != 2 || out.VoxelSize != 20 {
		t.Fatalf("box %dx%dx%d vs=%v", out.DimX, out.DimY, out.DimZ, out.VoxelSize)
	}
	if out.OriginX != 1000-320+200 || out.OriginY != -500-320+400 || out.OriginZ != 64-320+100 {
		t.Fatalf("origin (%v,%v,%v)", out.OriginX, out.OriginY, out.OriginZ)
	}
	raw, err := base64.StdEncoding.DecodeString(out.Arrival)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 12 {
		t.Fatalf("%d bytes, want 12", len(raw))
	}
	at := func(i, j, k int) byte { return raw[(k*out.DimY+j)*out.DimX+i] }
	if at(0, 0, 0) != 0 || at(2, 0, 0) != 40 || at(0, 1, 1) != arrMax {
		t.Fatalf("arrivals %d %d %d, want 0 40 %d", at(0, 0, 0), at(2, 0, 0), at(0, 1, 1), arrMax)
	}
	if at(1, 0, 0) != arrNever || at(2, 1, 1) != arrNever {
		t.Fatal("cells the cloud never reached should be 255")
	}
}

func TestReconstructSmokeVoxels(t *testing.T) {
	buf := readVoxelDump(t, "smoke_voxels_inferno_4019.bin")
	det := r3.Vector{X: 100, Y: 200, Z: 30}
	// A frame boundary past frame 0, which holds the sources.
	_, mid, _ := decodeVoxelFrames(buf[:len(buf)*3/4])

	// Bytes past the last sample were never seen, and a sample that does not
	// grow the log reads nothing, as in poll.
	partial, err := ReconstructSmokeVoxels(buf, [][2]int{{mid, 0}, {mid, 9}}, det, 64)
	if err != nil {
		t.Fatal(err)
	}
	if partial.EndTick != 9 {
		t.Errorf("end tick %d, want the last sample's 9", partial.EndTick)
	}
	whole, err := ReconstructSmokeVoxels(buf, [][2]int{{mid, 0}, {len(buf), 9}}, det, 64)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Arrival == whole.Arrival {
		t.Error("reading the rest of the log should change the cloud")
	}
	// A missing tick rate is 64, as in the demo path.
	if def, _ := ReconstructSmokeVoxels(buf, [][2]int{{mid, 0}, {len(buf), 9}}, det, 0); def != whole {
		t.Error("tick rate 0 should mean 64")
	}

	// A frame that fails to decode stops the read: what came before it stays,
	// and nothing after it is read.
	broken := append(append([]byte{}, buf[:mid]...), 0, 0, 2, 0, 0, 2)
	broken = append(broken, buf[mid:]...)
	got, err := ReconstructSmokeVoxels(broken, [][2]int{{len(broken), 0}}, det, 64)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := ReconstructSmokeVoxels(buf[:mid], [][2]int{{mid, 0}}, det, 64); got != want {
		t.Error("a log that breaks part way should keep exactly the frames before the break")
	}

	for name, c := range map[string]struct {
		buf     []byte
		samples [][2]int
		det     r3.Vector
		want    error
	}{
		"empty log":      {nil, [][2]int{{0, 0}}, det, ErrNoSmokeVoxels},
		"no samples":     {buf, nil, det, ErrNoSmokeVoxels},
		"no detonation":  {buf, [][2]int{{len(buf), 0}}, r3.Vector{}, ErrNoSmokeVoxels},
		"past the log":   {buf, [][2]int{{len(buf) + 1, 0}}, det, nil},
		"tick goes back": {buf, [][2]int{{mid, 5}, {len(buf), 4}}, det, nil},
		"negative tick":  {buf, [][2]int{{len(buf), -1}}, det, nil},
		"oversized log":  {make([]byte, voxelFrameDataSlots+1), nil, det, nil},
		"tick too late":  {buf, [][2]int{{len(buf), maxReconstructTick + 1}}, det, nil},
	} {
		_, err := ReconstructSmokeVoxels(c.buf, c.samples, c.det, 64)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || (c.want == nil && errors.Is(err, ErrNoSmokeVoxels)) {
			t.Errorf("%s: err %v, want %v", name, err, c.want)
		}
	}
	if _, err := ReconstructSmokeVoxels(buf, [][2]int{{len(buf), 0}}, det, 1e300); err == nil || errors.Is(err, ErrNoSmokeVoxels) {
		t.Errorf("an absurd tick rate: err %v, want a bad request", err)
	}
}

// A request's log can put a source point anywhere a byte reaches; one past
// the 32^3 grid must be skipped, not index past it.
func TestReconstructSmokeVoxelsIgnoresSourcesOutsideTheGrid(t *testing.T) {
	brick := voxelBrick{Index: 0, Mask: 0}
	buf := voxelFrameBytes(0, []voxelSource{{X: 255, Y: 255, Z: 255}, {X: 1, Y: 1, Z: 1}}, []voxelBrick{brick})
	det := r3.Vector{X: 1, Y: 2, Z: 3}
	out, err := ReconstructSmokeVoxels(buf, [][2]int{{len(buf), 0}}, det, 64)
	if err != nil {
		t.Fatal(err)
	}
	if out.DimX == 0 {
		t.Error("the in-grid source should still fill its brick")
	}
	only := voxelFrameBytes(0, []voxelSource{{X: 200, Y: 40, Z: 33}}, []voxelBrick{brick})
	if _, err := ReconstructSmokeVoxels(only, [][2]int{{len(only), 0}}, det, 64); !errors.Is(err, ErrNoSmokeVoxels) {
		t.Errorf("sources all outside the grid: err %v, want ErrNoSmokeVoxels", err)
	}
}

func TestExportVoxelsEmpty(t *testing.T) {
	arrival := make([]int, voxelCells)
	for i := range arrival {
		arrival[i] = -1
	}
	if _, ok := exportVoxels(arrival, r3.Vector{X: 1}, 64); ok {
		t.Fatal("an empty flood should not export")
	}
}
