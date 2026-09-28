package geometry

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/geo/r3"
)

// fakeMapsHost stands in for demo-dl.5stack.gg/maps: latest.json, manifests
// and meshes by key, everything else 404, a per-key hit count, and keys that
// can be held open to model a slow CDN.
type fakeMapsHost struct {
	mu     sync.Mutex
	files  map[string][]byte
	status map[string]int
	hits   map[string]int
	holds  map[string]chan struct{}
}

func newFakeMapsHost(t *testing.T) *fakeMapsHost {
	t.Helper()
	h := &fakeMapsHost{
		files:  map[string][]byte{},
		status: map[string]int{},
		hits:   map[string]int{},
		holds:  map[string]chan struct{}{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		h.mu.Lock()
		h.hits[key]++
		hold := h.holds[key]
		h.mu.Unlock()
		if hold != nil {
			<-hold
		}
		h.mu.Lock()
		code := h.status[key]
		body, ok := h.files[key]
		h.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	previous := mapsHost
	mapsHost = srv.URL
	t.Cleanup(func() { mapsHost = previous })

	t.Setenv("MAP_MESH_CDN", "")
	if err := os.Unsetenv("MAP_MESH_CDN"); err != nil {
		t.Fatal(err)
	}
	resetCache()
	t.Cleanup(resetCache)
	return h
}

func (h *fakeMapsHost) mesh(key string, tris ...[3]r3.Vector) {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	_, _ = zw.Write(triBlob(tris...))
	_ = zw.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[key] = out.Bytes()
}

func (h *fakeMapsHost) json(t *testing.T, key string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[key] = raw
}

func (h *fakeMapsHost) fail(key string, code int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status[key] = code
}

func (h *fakeMapsHost) hold(key string) (release func()) {
	ch := make(chan struct{})
	h.mu.Lock()
	h.holds[key] = ch
	h.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func (h *fakeMapsHost) count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[key]
}

func (h *fakeMapsHost) countMatching(fragment string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for key, hits := range h.hits {
		if strings.Contains(key, fragment) {
			n += hits
		}
	}
	return n
}

var (
	testFloor = [][3]r3.Vector{
		{{X: -500, Y: -500, Z: 0}, {X: 500, Y: -500, Z: 0}, {X: 500, Y: 500, Z: 0}},
		{{X: -500, Y: -500, Z: 0}, {X: 500, Y: 500, Z: 0}, {X: -500, Y: 500, Z: 0}},
	}
	testClipWall = [][3]r3.Vector{
		{{X: 100, Y: -500, Z: -10}, {X: 100, Y: 500, Z: -10}, {X: 100, Y: 500, Z: 500}},
		{{X: 100, Y: -500, Z: -10}, {X: 100, Y: 500, Z: 500}, {X: 100, Y: -500, Z: 500}},
	}
)

// publishBuilds lays out what the publisher leaves behind after a rebuild:
//
//   - de_test's hull is unchanged since 25000000, so both 25537370 manifests
//     point at the older object, and only 25537370 carries grenade clips;
//   - de_new failed in 25537370's first manifest and was rebuilt into a second
//     revision, which is what latest.json names;
//   - de_old exists only as a flat file of the pinned pre-manifest build.
func publishBuilds(t *testing.T, h *fakeMapsHost) {
	t.Helper()
	h.mesh("25000000/de_test.tri.gz", testFloor...)
	h.mesh("25537370/de_test.grenadeclip.tri.gz", testClipWall...)
	h.mesh("25537370/de_new.tri.gz", testFloor...)
	h.mesh(pinnedBuild+"/de_old.tri.gz", testFloor...)
	h.mesh(pinnedBuild+"/de_partial.tri.gz", testFloor...)
	h.mesh("25537370/de_partial.grenadeclip.tri.gz", testClipWall...)

	deTest := map[string]any{
		"tri":         "25000000/de_test.tri.gz",
		"grenadeclip": "25537370/de_test.grenadeclip.tri.gz",
		"view":        "25537370/de_test.view.bin.gz",
		"callouts":    "25537370/de_test.callouts.json",
		"sha256":      map[string]string{"tri": "x"},
	}
	h.json(t, "25537370/manifest.json", map[string]any{
		"version": 1,
		"build":   "25537370",
		"maps":    map[string]any{"de_test": deTest},
		"failed":  []string{"de_new"},
	})
	h.json(t, "25537370/manifest.r2.json", map[string]any{
		"version": 1,
		"build":   "25537370",
		"maps": map[string]any{
			"de_test": deTest,
			"de_new":  map[string]any{"tri": "25537370/de_new.tri.gz"},
			"de_partial": map[string]any{
				"grenadeclip": "25537370/de_partial.grenadeclip.tri.gz",
				"callouts":    "25537370/de_partial.callouts.json",
			},
		},
		"failed":      []string{},
		"failed_view": []string{"de_test"},
	})
	h.json(t, "latest.json", map[string]any{
		"version":  1,
		"build":    "25537370",
		"manifest": "25537370/manifest.r2.json",
	})
}

func TestDefaultLoadResolvesThroughLatestManifest(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)

	mesh, err := Load("de_test")
	if err != nil || mesh == nil {
		t.Fatalf("Load(de_test) = %v, %v; want a mesh", mesh, err)
	}
	if mesh.Triangles() != len(testFloor) {
		t.Fatalf("got %d triangles, want the %d-triangle hull", mesh.Triangles(), len(testFloor))
	}
	if got := h.count("25000000/de_test.tri.gz"); got != 1 {
		t.Fatalf("the deduped hull should be read from the key the manifest names, fetched %d times", got)
	}
	if got := h.count("25537370/de_test.tri.gz"); got != 0 {
		t.Fatalf("the new build's own directory holds no hull and must not be asked, asked %d times", got)
	}

	if rebuilt, err := Load("de_new"); err != nil || rebuilt == nil {
		t.Fatalf("Load(de_new) = %v, %v; the revision latest.json names lists it", rebuilt, err)
	}
	if got := h.count("25537370/manifest.json"); got != 0 {
		t.Fatalf("latest.json names manifest.r2.json; the first revision must not be assumed, fetched %d times", got)
	}

	for i := 0; i < 3; i++ {
		if again, _ := Load("de_test"); again != mesh {
			t.Fatal("a second Load should come out of the cache")
		}
	}
	if got := h.count("latest.json"); got != 1 {
		t.Fatalf("latest.json should be cached between loads, fetched %d times", got)
	}
	if got := h.count("25537370/manifest.r2.json"); got != 1 {
		t.Fatalf("the manifest should be cached, fetched %d times", got)
	}
	if got, err := ResolveMeshRevision(""); err != nil || got != "25537370" {
		t.Fatalf("ResolveMeshRevision(\"\") = %q, %v; want the build latest.json names", got, err)
	}
}

// All three consumers treat a map the manifest does not list the same way: the
// pinned build's flat hull, and no grenade clips.
func TestMapMissingFromManifestFallsBackToPinnedBuild(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)

	old, err := Load("de_old")
	if err != nil || old == nil {
		t.Fatalf("Load(de_old) = %v, %v; want the pinned build's flat hull", old, err)
	}
	world, err := LoadGrenadeWorld("de_old", "")
	if err != nil || world == nil {
		t.Fatalf("LoadGrenadeWorld(de_old) = %v, %v", world, err)
	}
	if world.Hull() != old || world.GrenadeClipTriangles() != 0 {
		t.Fatalf("an unlisted map is the pinned hull with no clips, got %d clip triangles", world.GrenadeClipTriangles())
	}
	if got := h.countMatching("de_old.grenadeclip"); got != 0 {
		t.Fatalf("an unlisted map has no grenade clips to look for, asked %d times", got)
	}

	for i := 0; i < 2; i++ {
		if absent, err := Load("de_absent"); err != nil || absent != nil {
			t.Fatalf("a map on neither the manifest nor the pinned build has no mesh, got %v, %v", absent, err)
		}
	}
	if got := h.count(pinnedBuild + "/de_absent.tri.gz"); got != 1 {
		t.Fatalf("the pinned probe should be made once and remembered, made %d times", got)
	}
}

// The fallback is per asset: an entry that exists but has no tri still gets the
// pinned build's flat hull, while its grenade clips come from the manifest.
func TestEntryWithoutTriKeepsThePinnedHull(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)

	world, err := LoadGrenadeWorld("de_partial", "")
	if err != nil || world == nil {
		t.Fatalf("LoadGrenadeWorld(de_partial) = %v, %v", world, err)
	}
	if got := h.count(pinnedBuild + "/de_partial.tri.gz"); got != 1 {
		t.Fatalf("an entry without a tri should read the pinned hull, read it %d times", got)
	}
	if world.GrenadeClipTriangles() != len(testClipWall) {
		t.Fatalf("the entry's own grenade clips should still load, got %d triangles", world.GrenadeClipTriangles())
	}
	if hull, err := Load("de_partial"); err != nil || hull != world.Hull() {
		t.Fatalf("Load(de_partial) = %v, %v; want the same pinned hull", hull, err)
	}
}

// A build id names its first manifest revision; a build from before manifests
// existed keeps the flat layout.
func TestBuildRevisionUsesItsFirstManifest(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)

	current, err := LoadRevision("de_test", "25537370")
	if err != nil || current == nil {
		t.Fatalf("LoadRevision(de_test, 25537370) = %v, %v", current, err)
	}
	if def, _ := Load("de_test"); def != current {
		t.Fatal("two manifests sharing one deduped object should share one built mesh")
	}
	if missing, err := LoadRevision("de_new", "25537370"); err != nil || missing != nil {
		t.Fatalf("de_new failed in the first revision, so the build id has only the pinned probe; got %v, %v", missing, err)
	}
	if got := h.count("25537370/manifest.json"); got != 1 {
		t.Fatalf("a build id should read <build>/manifest.json, read %d times", got)
	}

	flat, err := LoadRevision("de_old", pinnedBuild)
	if err != nil || flat == nil {
		t.Fatalf("LoadRevision(de_old, %s) = %v, %v", pinnedBuild, flat, err)
	}
	if h.count(pinnedBuild+"/manifest.json") != 1 {
		t.Fatal("the old build's manifest should have been looked for")
	}
}

func TestLatestUnreachableFallsBackToPinnedBuild(t *testing.T) {
	h := newFakeMapsHost(t)
	h.mesh(pinnedBuild+"/de_test.tri.gz", testFloor...)
	h.fail("latest.json", http.StatusBadGateway)

	for i := 0; i < 3; i++ {
		mesh, err := Load("de_test")
		if err != nil || mesh == nil {
			t.Fatalf("Load(de_test) with latest.json down = %v, %v; want the pinned build's mesh", mesh, err)
		}
	}
	if got := h.count("latest.json"); got != 1 {
		t.Fatalf("a failed latest.json should be retried after a pause, not on every load; fetched %d times", got)
	}
	if got, _ := ResolveMeshRevision(""); got != pinnedBuild {
		t.Fatalf("ResolveMeshRevision(\"\") = %q, want the pinned build %s", got, pinnedBuild)
	}
}

func TestUnknownIndexVersionFallsBackLikeAnOutage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, h *fakeMapsHost)
	}{
		{"latest.json", func(t *testing.T, h *fakeMapsHost) {
			h.json(t, "latest.json", map[string]any{
				"version": 2, "build": "25537370", "manifest": "25537370/manifest.r2.json",
			})
		}},
		{"manifest", func(t *testing.T, h *fakeMapsHost) {
			h.json(t, "25537370/manifest.r2.json", map[string]any{
				"version": 2, "build": "25537370", "maps": map[string]any{},
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeMapsHost(t)
			publishBuilds(t, h)
			tc.mutate(t, h)

			if got, _ := ResolveMeshRevision(""); got != pinnedBuild {
				t.Fatalf("an unknown %s version should fall back to the pinned build, got %q", tc.name, got)
			}
			if mesh, err := Load("de_old"); err != nil || mesh == nil {
				t.Fatalf("Load(de_old) = %v, %v; want the pinned build's mesh", mesh, err)
			}
		})
	}

	h := newFakeMapsHost(t)
	publishBuilds(t, h)
	h.json(t, "25537370/manifest.json", map[string]any{"version": 2, "build": "25537370", "maps": map[string]any{}})
	if _, err := LoadRevision("de_test", "25537370"); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("a build revision with an unknown manifest version should say so, got %v", err)
	}
}

func TestLatestKeepsLastGoodPointerWhenRefreshFails(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)

	if mesh, err := Load("de_test"); err != nil || mesh == nil {
		t.Fatalf("Load(de_test) = %v, %v", mesh, err)
	}
	h.fail("latest.json", http.StatusBadGateway)
	latest.expire()

	if got, _ := ResolveMeshRevision(""); got != "25537370" {
		t.Fatalf("an expired pointer should be served while it refreshes, got %q", got)
	}
	latest.settle()
	if got, _ := ResolveMeshRevision(""); got != "25537370" {
		t.Fatalf("a failed refresh should keep the last good build, got %q", got)
	}
	if got := h.count("latest.json"); got != 2 {
		t.Fatalf("the expired pointer should have been refreshed once, fetched %d times", got)
	}
	if mesh, err := Load("de_new"); err != nil || mesh == nil {
		t.Fatalf("Load(de_new) after a failed refresh = %v, %v; the last manifest still lists it", mesh, err)
	}
}

func TestSlowLatestRefreshDoesNotBlockLoads(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)

	mesh, err := Load("de_test")
	if err != nil || mesh == nil {
		t.Fatalf("Load(de_test) = %v, %v", mesh, err)
	}
	release := h.hold("latest.json")
	t.Cleanup(release)
	latest.expire()

	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if again, err := Load("de_test"); err != nil || again != mesh {
				t.Errorf("Load during a slow refresh = %v, %v; want the cached mesh", again, err)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loads waited on a latest.json refresh that was still in flight")
	}

	release()
	latest.settle()
	if got := h.count("latest.json"); got != 2 {
		t.Fatalf("concurrent loads should share one refresh, latest.json fetched %d times", got)
	}
}

func TestManifestKeysCannotLeaveTheMapsRoot(t *testing.T) {
	h := newFakeMapsHost(t)
	h.json(t, "latest.json", map[string]any{"version": 1, "build": "1", "manifest": "1/manifest.json"})
	h.json(t, "1/manifest.json", map[string]any{
		"version": 1,
		"build":   "1",
		"maps": map[string]any{
			"de_up":   map[string]any{"tri": "../secret.tri.gz"},
			"de_abs":  map[string]any{"tri": "/etc/passwd"},
			"de_host": map[string]any{"tri": "https://elsewhere.test/x.tri.gz"},
		},
	})
	for _, name := range []string{"de_up", "de_abs", "de_host"} {
		if mesh, err := Load(name); err != nil || mesh != nil {
			t.Fatalf("Load(%s) = %v, %v; an unsafe manifest key must resolve to no mesh", name, mesh, err)
		}
		if got := h.count(pinnedBuild + "/" + name + ".tri.gz"); got != 1 {
			t.Fatalf("an unusable key should be treated as unlisted and probe the pinned build, probed %d times", got)
		}
	}
	for _, key := range []string{"secret.tri.gz", "etc/passwd", "x.tri.gz"} {
		if got := h.countMatching(key); got != 0 {
			t.Fatalf("an unsafe key reached the host as %q", key)
		}
	}
}

// A file the manifest lists is a promise: a 403 or 404 for it is a broken or
// not-yet-visible publish, retried on the next load, never remembered as
// "this map has no mesh".
func TestRefusedListedFilesAreErrorsNotAbsences(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			h := newFakeMapsHost(t)
			publishBuilds(t, h)

			h.fail("25000000/de_test.tri.gz", code)
			if mesh, err := Load("de_test"); err == nil {
				t.Fatalf("a refused listed hull should be an error, got %v", mesh)
			}
			if got := cacheEntryCount(); got != 0 {
				t.Fatalf("a refused listed hull must not be memoized, cache holds %d entries", got)
			}
			h.fail("25000000/de_test.tri.gz", 0)
			if mesh, err := Load("de_test"); err != nil || mesh == nil {
				t.Fatalf("the hull should load once the CDN serves it, got %v, %v", mesh, err)
			}

			h.fail("25537370/de_test.grenadeclip.tri.gz", code)
			if world, err := LoadGrenadeWorld("de_test", ""); err == nil {
				t.Fatalf("a refused listed grenade clip should be an error, got %d clip triangles",
					world.GrenadeClipTriangles())
			}
			h.fail("25537370/de_test.grenadeclip.tri.gz", 0)
			world, err := LoadGrenadeWorld("de_test", "")
			if err != nil || world.GrenadeClipTriangles() != len(testClipWall) {
				t.Fatalf("the grenade clip should load once the CDN serves it, got %v", err)
			}
		})
	}
}

func TestGrenadeClipsAreInvisibleToLineOfSight(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)
	t.Setenv("MAP_MESH_CACHE", "1")

	eye := r3.Vector{X: 0, Y: 0, Z: 50}
	across := r3.Vector{X: 200, Y: 0, Z: 50}

	hull, err := Load("de_test")
	if err != nil || hull == nil {
		t.Fatalf("Load(de_test) = %v, %v", hull, err)
	}
	if hull.Occluded(eye, across) {
		t.Fatal("a grenade clip must not block line of sight")
	}
	if _, ok := hull.RayHitSurface(eye, r3.Vector{X: 1}); ok {
		t.Fatal("the LOS mesh must not contain the grenade clip")
	}

	world, err := LoadGrenadeWorld("de_test", "")
	if err != nil || world == nil {
		t.Fatalf("LoadGrenadeWorld(de_test) = %v, %v", world, err)
	}
	if world.Hull() != hull {
		t.Fatal("the grenade world should share the LOS hull, not build a second copy")
	}
	if world.Revision() != "25537370" {
		t.Fatalf("the world should name the build it was loaded at, got %q", world.Revision())
	}
	if world.GrenadeClipTriangles() != len(testClipWall) {
		t.Fatalf("grenade world carries %d clip triangles, want %d", world.GrenadeClipTriangles(), len(testClipWall))
	}
	hit, ok := world.RayHitSurface(eye, r3.Vector{X: 1})
	if !ok || hit.Distance < 99 || hit.Distance > 101 {
		t.Fatalf("a grenade should hit the clip 100 units out, got %+v, %v", hit, ok)
	}
	if down, ok := world.RayHitSurface(eye, r3.Vector{Z: -1}); !ok || down.Distance < 49 || down.Distance > 51 {
		t.Fatalf("the hull should still be hit straight down, got %+v, %v", down, ok)
	}
	if _, max, ok := world.Bounds(); !ok || max.Z < 500 {
		t.Fatalf("the grenade world's bounds should cover the clip, got max %v", max)
	}

	if _, err := LoadGrenadeWorld("de_test", ""); err != nil {
		t.Fatal(err)
	}
	if got := h.count("25000000/de_test.tri.gz"); got != 1 {
		t.Fatalf("a clip mesh must not evict the hull from a cache of one, hull fetched %d times", got)
	}
	if got := h.count("25537370/de_test.grenadeclip.tri.gz"); got != 1 {
		t.Fatalf("the clip mesh should be cached too, fetched %d times", got)
	}
}

func TestGrenadeWorldWithoutPublishedClipsIsTheHull(t *testing.T) {
	fetches := serveMeshes(t, "de_x")

	for i := 0; i < 2; i++ {
		world, err := LoadGrenadeWorld("de_x", "")
		if err != nil || world == nil {
			t.Fatalf("LoadGrenadeWorld(de_x) = %v, %v", world, err)
		}
		if world.GrenadeClipTriangles() != 0 || world.Triangles() != 1 {
			t.Fatalf("a build without grenade clips is the hull alone, got %d/%d triangles",
				world.Triangles(), world.GrenadeClipTriangles())
		}
	}
	if got := fetches.count("de_x.grenadeclip"); got != 1 {
		t.Fatalf("a missing grenade clip should be looked for once, asked %d times", got)
	}
	if world, err := LoadGrenadeWorld("de_missing", ""); err != nil || world != nil {
		t.Fatalf("no hull means no grenade world, got %v, %v", world, err)
	}
}

// Flying without clips because their fetch failed would read as drift, so the
// failure has to surface instead.
func TestGrenadeClipFetchFailureIsAnError(t *testing.T) {
	h := newFakeMapsHost(t)
	publishBuilds(t, h)
	h.fail("25537370/de_test.grenadeclip.tri.gz", http.StatusBadGateway)

	if world, err := LoadGrenadeWorld("de_test", ""); err == nil {
		t.Fatalf("a failed clip fetch should be an error, got a world with %d clip triangles",
			world.GrenadeClipTriangles())
	}
}
