package main

import (
	"bytes"
	"compress/bzip2"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// multiblockDemo is what testdata/multiblock.dem.bz2 decompresses to: a demo
// magic and enough lines to fill several 100k bzip2 blocks, so the parallel
// decoder has blocks to reassemble in order.
func multiblockDemo() []byte {
	var b bytes.Buffer
	b.WriteString("PBDEMS2\x00")
	for i := 0; i < 40000; i++ {
		fmt.Fprintf(&b, "tick %d frame %d\n", i%50, i%7)
	}
	return b.Bytes()
}

func TestSniffDecodesMultiBlockBzip2(t *testing.T) {
	compressed, err := os.ReadFile(filepath.Join("testdata", "multiblock.dem.bz2"))
	if err != nil {
		t.Fatal(err)
	}
	want := multiblockDemo()
	stdlib, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(compressed)))
	if err != nil || !bytes.Equal(stdlib, want) {
		t.Fatalf("fixture does not decompress to the expected demo (err %v)", err)
	}

	r, err := sniffDemoStream(context.Background(), bytes.NewReader(compressed), "test")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("parallel decode gave %d bytes, want the %d the standard library gives", len(got), len(want))
	}
}

// The reader must hand back exactly what was downloaded while the download is
// still arriving, waiting at its end rather than reporting EOF early.
func TestSpoolReaderFollowsTheDownload(t *testing.T) {
	want := multiblockDemo()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(want)))
		for off := 0; off < len(want); off += 64 << 10 {
			_, _ = w.Write(want[off:min(off+64<<10, len(want))])
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer srv.Close()

	dl, _, err := startDownload(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.close()
	got, err := io.ReadAll(dl.reader())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes, want %d", len(got), len(want))
	}
	if res := dl.wait(); res.err != nil || res.size != int64(len(want)) {
		t.Fatalf("download ended with %v after %d bytes", res.err, res.size)
	}
}

// truncatedServer promises more bytes than it sends, the way a CDN dropping
// the connection mid-stream looks to the client.
func truncatedServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)+4096))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSpoolReaderSurfacesABrokenDownload(t *testing.T) {
	srv := truncatedServer(t, multiblockDemo()[:100000])
	dl, _, err := startDownload(srv.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.close()
	if _, err := io.ReadAll(dl.reader()); err == nil {
		t.Fatal("a download that broke off read back as a complete demo")
	}
	if res := dl.wait(); res.err == nil || !res.retryable {
		t.Fatalf("download ended with %v (retryable %v), want a retryable error", res.err, res.retryable)
	}
}

// The parser returns what it had when its input fails, so a parse of a broken
// download must be discarded and the download retried, never answered.
func TestBrokenDownloadIsNeverAResult(t *testing.T) {
	srv := truncatedServer(t, append([]byte("PBDEMS2\x00"), make([]byte, 50000)...))
	result, _, retryable, err := fetchAndParseOnce(srv.URL, "test")
	if err == nil || result != nil {
		t.Fatalf("got a result (%v) from a truncated download", result != nil)
	}
	if !retryable {
		t.Fatalf("a truncated download should be retried: %v", err)
	}
}

func TestExpiredDemoIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, _, retryable, err := fetchAndParseOnce(srv.URL, "test")
	if err == nil || retryable {
		t.Fatalf("a 404 gave %v (retryable %v), want a permanent error", err, retryable)
	}
}

// A CDN error page downloads fine and is not a demo: a 422, not a retry.
func TestNonDemoBodyIsAParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>gone</html>"))
	}))
	defer srv.Close()
	_, _, retryable, err := fetchAndParseOnce(srv.URL, "test")
	var pe parseError
	if !errors.As(err, &pe) || retryable {
		t.Fatalf("got %v (retryable %v), want a parse error", err, retryable)
	}
}
