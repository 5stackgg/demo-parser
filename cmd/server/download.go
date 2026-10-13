package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

var demoClient = &http.Client{Timeout: 5 * time.Minute}

// One in-flight download per CDN host. Valve's replay servers 502 under
// concurrent load, so requests to the same replayNNN.valve.net host are
// serialized while different hosts still download in parallel.
var (
	hostLocksMu sync.Mutex
	hostLocks   = map[string]*sync.Mutex{}
)

func hostLock(host string) *sync.Mutex {
	hostLocksMu.Lock()
	defer hostLocksMu.Unlock()
	l := hostLocks[host]
	if l == nil {
		l = &sync.Mutex{}
		hostLocks[host] = l
	}
	return l
}

// spool is a demo download landing in a temp file while the parser reads it
// back from the front. The download still runs at network speed, exactly as it
// did when the parse waited for it to finish; the reader follows behind and
// waits at the end of whatever has arrived. Reading the response body at the
// parser's pace instead would hold the host lock for the whole parse and give
// Valve's CDN, which drops connections mid-stream, that much longer to drop
// this one.
type spool struct {
	file   *os.File
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	grew    *sync.Cond
	written int64
	ended   bool
	result  downloadResult
}

// downloadResult is how a download ended.
type downloadResult struct {
	// err is why the download stopped short; nil once it completed.
	err       error
	retryable bool
	size      int64
	elapsed   time.Duration
}

// startDownload requests the demo and, once the server has answered with a
// body, copies it into a temp file in the background. An error here means no
// body: retryable for transport failures, 5xx and 429; a 4xx (an expired
// demo) is permanent.
func startDownload(demoURL, logPrefix string) (*spool, bool, error) {
	start := time.Now()
	var lock *sync.Mutex
	if u, err := url.Parse(demoURL); err == nil && u.Host != "" {
		lock = hostLock(u.Host)
		lock.Lock()
	}
	unlock := func() {
		if lock != nil {
			lock.Unlock()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, demoURL, nil)
	if err != nil {
		cancel()
		unlock()
		return nil, false, fmt.Errorf("fetch demo: %v", err)
	}
	resp, err := demoClient.Do(req)
	if err != nil {
		cancel()
		unlock()
		return nil, true, fmt.Errorf("fetch demo: %v", err)
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		cancel()
		unlock()
		retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, retryable, fmt.Errorf("fetch demo: upstream %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "demo-*.dem")
	if err != nil {
		resp.Body.Close()
		cancel()
		unlock()
		return nil, false, fmt.Errorf("create temp: %v", err)
	}
	s := &spool{file: f, cancel: cancel, done: make(chan struct{})}
	s.grew = sync.NewCond(&s.mu)
	go func() {
		defer close(s.done)
		defer unlock()
		defer resp.Body.Close()
		retryable, err := s.fill(resp.Body, resp.ContentLength)
		if err == nil {
			// Logged as it lands rather than when the parse ends, so the
			// timestamp says when the download actually finished.
			log.Printf("[%s] downloaded %d bytes in %s", logPrefix, s.size(), time.Since(start))
		}
		s.mu.Lock()
		s.ended = true
		s.result = downloadResult{err: err, retryable: retryable, size: s.written, elapsed: time.Since(start)}
		s.grew.Broadcast()
		s.mu.Unlock()
	}()
	return s, false, nil
}

// fill copies the body into the file, publishing each chunk to the reader as
// it lands.
func (s *spool) fill(body io.Reader, want int64) (retryable bool, err error) {
	buf := make([]byte, 256<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := s.file.Write(buf[:n]); werr != nil {
				return false, fmt.Errorf("write temp: %v", werr)
			}
			s.mu.Lock()
			s.written += int64(n)
			s.grew.Broadcast()
			s.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return true, fmt.Errorf("download demo (after %d bytes): %w", s.size(), rerr)
		}
	}
	if got := s.size(); want > 0 && got != want {
		return true, fmt.Errorf("download truncated: got %d of %d bytes", got, want)
	}
	return false, nil
}

func (s *spool) size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// reader reads the demo from the start, blocking at the end of what has
// arrived until more does. A download that fails surfaces as that error rather
// than as the end of the file, so a parse of it can never pass for a whole one.
func (s *spool) reader() io.Reader {
	return &spoolReader{s: s}
}

type spoolReader struct {
	s   *spool
	off int64
}

func (r *spoolReader) Read(p []byte) (int, error) {
	s := r.s
	s.mu.Lock()
	for r.off >= s.written && !s.ended {
		s.grew.Wait()
	}
	avail, err := s.written-r.off, s.result.err
	s.mu.Unlock()
	if avail <= 0 {
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > avail {
		p = p[:avail]
	}
	n, err := s.file.ReadAt(p, r.off)
	r.off += int64(n)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}

// wait blocks until the download has ended and reports how.
func (s *spool) wait() downloadResult {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// close abandons the download if it is still running and removes the file.
func (s *spool) close() {
	s.cancel()
	<-s.done
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
}
