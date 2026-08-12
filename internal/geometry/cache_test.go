package geometry

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
)

// serveMeshes stands in for the mesh CDN: any `<name>.tri` in the set is
// served as a one-triangle blob, anything else 404s the way a map with no
// published mesh does. Returns the number of times each key was fetched, so a
// test can tell a cache hit from a rebuild.
func serveMeshes(t *testing.T, have ...string) map[string]int {
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

	fetches := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".tri")
		fetches[key]++
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

	return fetches
}

func resetCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = map[string]*cached{}
	lru = nil
}

func cachedMeshCount() int {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	return len(lru)
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
	if fetches["de_a"] != 1 {
		t.Fatalf("de_a should still have been cached, fetched %d times", fetches["de_a"])
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
	if fetches["de_a"] != 1 {
		t.Errorf("de_a was most recently used and should have survived, fetched %d times", fetches["de_a"])
	}
	if _, err := Load("de_b"); err != nil {
		t.Fatalf("Load(de_b) after eviction: %v", err)
	}
	if fetches["de_b"] != 2 {
		t.Errorf("de_b was least recently used and should have been evicted and refetched, fetched %d times", fetches["de_b"])
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
	for i := 0; i < 3; i++ {
		mesh, err := Load(fmt.Sprintf("workshop/%d/de_missing", i))
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
	if fetches["de_missing0"] != 1 {
		t.Errorf("a missing mesh should be fetched at most once, got %d", fetches["de_missing0"])
	}

	if _, err := Load("de_real"); err != nil {
		t.Fatalf("Load(de_real) again: %v", err)
	}
	if fetches["de_real"] != 1 {
		t.Errorf("missing maps must not evict a real mesh; de_real fetched %d times", fetches["de_real"])
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
	if fetches["de_a"] != 2 {
		t.Errorf("with the cache disabled every Load should rebuild, fetched %d times", fetches["de_a"])
	}
	if got := cachedMeshCount(); got != 0 {
		t.Errorf("cache should hold nothing when disabled, holds %d", got)
	}
}
