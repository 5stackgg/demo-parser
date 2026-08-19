package geometry

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang/geo/r3"
)

type fetchLog struct {
	mu sync.Mutex
	n  map[string]int
}

func (f *fetchLog) record(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n[key]++
}

func (f *fetchLog) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n[key]
}

// serveMeshes stands in for the mesh CDN: any `<name>.tri` in the set is
// served as a one-triangle blob, anything else 404s the way a map with no
// published mesh does. The returned log counts fetches per key, so a test can
// tell a cache hit from a rebuild.
func serveMeshes(t *testing.T, have ...string) *fetchLog {
	t.Helper()

	published := map[string]bool{}
	for _, h := range have {
		published[h] = true
	}
	blob := triBlob([3]r3.Vector{
		{X: 0, Y: -50, Z: -50},
		{X: 0, Y: 50, Z: -50},
		{X: 0, Y: 50, Z: 50},
	})

	// Written from the server's per-connection goroutine, read from the test
	// goroutine, so it needs the lock even though Loads happen to be serial.
	f := &fetchLog{n: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".tri")
		f.record(key)
		if !published[key] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(blob)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MAP_MESH_CDN", srv.URL)

	// The cache is process-wide; keep tests from inheriting each other's state.
	resetCache()
	t.Cleanup(resetCache)

	return f
}

func resetCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = map[string]*cached{}
	lru = nil
}

// cachedMeshCount is the bound's own bookkeeping; cacheEntryCount is the map
// that actually holds memory. The two are kept in step by hand, in touch and
// Load, so tests assert on both — a leak that left entries in the map while the
// LRU list looked empty is exactly the failure this package is here to avoid.
func cachedMeshCount() int {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return len(lru)
}

func cacheEntryCount() int {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return len(cache)
}

func TestCacheEvictsLeastRecentlyUsedMesh(t *testing.T) {
	fetches := serveMeshes(t, "de_a", "de_b", "de_c")
	t.Setenv("MAP_MESH_CACHE", "2")

	for _, m := range []string{"de_a", "de_b"} {
		if mesh, err := Load(m); err != nil || mesh == nil {
			t.Fatalf("Load(%s) = %v, %v; want a mesh", m, mesh, err)
		}
	}
	// de_a is now the least recently used; re-touch it so de_b becomes the
	// eviction candidate instead.
	if _, err := Load("de_a"); err != nil {
		t.Fatalf("Load(de_a) again: %v", err)
	}
	if fetches.count("de_a") != 1 {
		t.Fatalf("de_a should still have been cached, fetched %d times", fetches.count("de_a"))
	}

	if _, err := Load("de_c"); err != nil {
		t.Fatalf("Load(de_c): %v", err)
	}
	if got := cachedMeshCount(); got != 2 {
		t.Fatalf("cache holds %d meshes, want the 2 it is bounded to", got)
	}
	if _, err := Load("de_a"); err != nil {
		t.Fatalf("Load(de_a) after eviction: %v", err)
	}
	if fetches.count("de_a") != 1 {
		t.Errorf("de_a was most recently used and should have survived, fetched %d times", fetches.count("de_a"))
	}
	if _, err := Load("de_b"); err != nil {
		t.Fatalf("Load(de_b) after eviction: %v", err)
	}
	if fetches.count("de_b") != 2 {
		t.Errorf("de_b was least recently used and should have been evicted and refetched, fetched %d times", fetches.count("de_b"))
	}
}

// A map with no published .tri memoizes as (nil, nil). Those entries are free,
// so they must neither be refetched nor push a real mesh out of the cache.
func TestMissingMeshIsMemoizedAndDoesNotEvict(t *testing.T) {
	fetches := serveMeshes(t, "de_real")
	t.Setenv("MAP_MESH_CACHE", "1")

	if mesh, err := Load("de_real"); err != nil || mesh == nil {
		t.Fatalf("Load(de_real) = %v, %v; want a mesh", mesh, err)
	}
	// Distinct workshop maps: normalizeMapName strips the workshop/<id>/ prefix,
	// so the trailing index is what keeps these three separate keys.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("workshop/%d/de_missing%d", i, i)
		mesh, err := Load(name)
		if err != nil {
			t.Fatalf("Load of an unpublished map should not error: %v", err)
		}
		if mesh != nil {
			t.Fatal("Load of an unpublished map should return a nil mesh")
		}
	}
	if _, err := Load("de_missing0"); err != nil {
		t.Fatalf("Load(de_missing0) again: %v", err)
	}
	if got := fetches.count("de_missing0"); got != 1 {
		t.Errorf("a missing mesh should be fetched at most once, got %d", got)
	}

	if _, err := Load("de_real"); err != nil {
		t.Fatalf("Load(de_real) again: %v", err)
	}
	if got := fetches.count("de_real"); got != 1 {
		t.Errorf("missing maps must not evict a real mesh; de_real fetched %d times", got)
	}
	if got := cachedMeshCount(); got != 1 {
		t.Errorf("only de_real should count against the bound, got %d", got)
	}
}

// A transient CDN failure must not be memoized: sync.Once would never run
// again, silently disabling sightline validation for that map for the life of
// the process.
func TestFailedLoadIsRetried(t *testing.T) {
	var mu sync.Mutex
	fail := true
	blob := triBlob([3]r3.Vector{
		{X: 0, Y: -50, Z: -50},
		{X: 0, Y: 50, Z: -50},
		{X: 0, Y: 50, Z: 50},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		down := fail
		mu.Unlock()
		if down {
			w.WriteHeader(http.StatusBadGateway) // the CDN having a bad minute
			return
		}
		_, _ = w.Write(blob)
	}))
	defer srv.Close()
	t.Setenv("MAP_MESH_CDN", srv.URL)
	resetCache()
	t.Cleanup(resetCache)

	if _, err := Load("de_flaky"); err == nil {
		t.Fatal("a 502 from the CDN should surface as an error")
	}
	if got := cacheEntryCount(); got != 0 {
		t.Errorf("a failed load must not stay memoized, cache holds %d entries", got)
	}

	mu.Lock()
	fail = false
	mu.Unlock()

	mesh, err := Load("de_flaky")
	if err != nil {
		t.Fatalf("retry after the CDN recovered: %v", err)
	}
	if mesh == nil {
		t.Fatal("retry after the CDN recovered should return a mesh")
	}
}

func TestCacheCanBeDisabled(t *testing.T) {
	fetches := serveMeshes(t, "de_a")
	t.Setenv("MAP_MESH_CACHE", "0")

	for i := 0; i < 2; i++ {
		if mesh, err := Load("de_a"); err != nil || mesh == nil {
			t.Fatalf("Load(de_a) = %v, %v; want a mesh even with caching off", mesh, err)
		}
	}
	if fetches.count("de_a") != 2 {
		t.Errorf("with the cache disabled every Load should rebuild, fetched %d times", fetches.count("de_a"))
	}
	if got := cachedMeshCount(); got != 0 {
		t.Errorf("cache should hold nothing when disabled, holds %d", got)
	}
	if got := cacheEntryCount(); got != 0 {
		t.Errorf("the entry map should be empty too when disabled, holds %d", got)
	}
}

func TestResolveMeshRevision(t *testing.T) {
	t.Setenv("MAP_MESH_CDN", "https://example.test/pinned/")
	for ref, want := range map[string]string{
		"":                                      "https://example.test/pinned",
		"https://mirror.test/meshes/":           "https://mirror.test/meshes",
		"http://127.0.0.1:8080":                 "http://127.0.0.1:8080",
		"17595823-5":                            "https://cdn.jsdelivr.net/gh/5stackgg/replay-map-meshes@17595823-5",
		"replay-map-meshes@17595823-5":          "https://cdn.jsdelivr.net/gh/5stackgg/replay-map-meshes@17595823-5",
		"5stackgg/replay-map-meshes@17595823-5": "https://cdn.jsdelivr.net/gh/5stackgg/replay-map-meshes@17595823-5",
		"someone-else/other-meshes@v1.2.3":      "https://cdn.jsdelivr.net/gh/someone-else/other-meshes@v1.2.3",
	} {
		got, err := ResolveMeshRevision(ref)
		if err != nil {
			t.Errorf("ResolveMeshRevision(%q): %v", ref, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveMeshRevision(%q) = %q, want %q", ref, got, want)
		}
	}
	// A revision comes off a request body, so anything that could walk out of
	// the pinned path has to be refused rather than pasted into a URL.
	for _, bad := range []string{
		"../../../etc", "tag/../..", "repo@tag/nested", "..", "a b", "tag?query=1",
		"owner/repo@", "@tag", "owner/repo/extra@tag",
	} {
		if got, err := ResolveMeshRevision(bad); err == nil {
			t.Errorf("ResolveMeshRevision(%q) should be refused, got %q", bad, got)
		}
	}
}

// Drift detection stands or falls on this: the same map at two revisions must
// be two cache entries, or the second load would hand back the first's mesh and
// every lineup would look unchanged.
func TestRevisionsAreSeparateCacheEntries(t *testing.T) {
	one := triBlob([3]r3.Vector{{X: 0, Y: -50, Z: -50}, {X: 0, Y: 50, Z: -50}, {X: 0, Y: 50, Z: 50}})
	two := append(append([]byte(nil), one...), triBlob(
		[3]r3.Vector{{X: 10, Y: -50, Z: -50}, {X: 10, Y: 50, Z: -50}, {X: 10, Y: 50, Z: 50}},
		[3]r3.Vector{{X: 20, Y: -50, Z: -50}, {X: 20, Y: 50, Z: -50}, {X: 20, Y: 50, Z: 50}},
	)...)

	serve := func(blob []byte) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(blob)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	oldRev, newRev := serve(one), serve(two)
	resetCache()
	t.Cleanup(resetCache)
	t.Setenv("MAP_MESH_CACHE", "4")

	before, err := LoadRevision("de_x", oldRev)
	if err != nil || before == nil {
		t.Fatalf("LoadRevision(old) = %v, %v", before, err)
	}
	after, err := LoadRevision("de_x", newRev)
	if err != nil || after == nil {
		t.Fatalf("LoadRevision(new) = %v, %v", after, err)
	}
	if before == after {
		t.Fatal("two revisions of one map came back as the same mesh")
	}
	if before.Triangles() != 1 || after.Triangles() != 3 {
		t.Fatalf("wrong geometry per revision: old %d triangles, new %d",
			before.Triangles(), after.Triangles())
	}
	if got := cachedMeshCount(); got != 2 {
		t.Fatalf("both revisions should be resident, cache holds %d", got)
	}
	// And each is still memoized on its own key.
	if again, _ := LoadRevision("de_x", oldRev); again != before {
		t.Fatal("the old revision should have come back out of the cache")
	}
}

func TestMaxCachedMeshesReportsTheBound(t *testing.T) {
	t.Setenv("MAP_MESH_CACHE", "7")
	if got := MaxCachedMeshes(); got != 7 {
		t.Fatalf("MaxCachedMeshes() = %d, want 7", got)
	}
}
