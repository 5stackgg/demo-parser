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
