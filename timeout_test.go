package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadResponseHeaderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := newQFileClient(100*time.Millisecond, "https://qfile.qq.com/q/test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	_, _, err = client.downloadFile(ctx, server.URL, filepath.Join(root, "file"), -1)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || ctx.Err() != nil {
		t.Fatalf("header timeout = %v, parent context = %v", err, ctx.Err())
	}
	assertEmptyDirectory(t, root)
}

func TestDownloadBodyIdleTimeoutCleansUp(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "x")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			client, err := newQFileClient(time.Second, "https://qfile.qq.com/q/test")
			if err != nil {
				t.Fatal(err)
			}
			client.downloadIdleTimeout = 150 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root := t.TempDir()
			_, _, err = client.downloadFile(ctx, server.URL, filepath.Join(root, "file"), 2)
			if !errors.Is(err, errDownloadIdleTimeout) || ctx.Err() != nil {
				t.Fatalf("idle timeout = %v, parent context = %v", err, ctx.Err())
			}
			if status != http.StatusOK && !strings.Contains(err.Error(), "503") {
				t.Fatalf("lost error status: %v", err)
			}
			assertEmptyDirectory(t, root)
		})
	}
}

func TestActiveDownloadOutlivesIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for index := 0; index < 4; index++ {
			if index > 0 {
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-r.Context().Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
		}
	}))
	defer server.Close()
	client, err := newQFileClient(time.Second, "https://qfile.qq.com/q/test")
	if err != nil {
		t.Fatal(err)
	}
	client.downloadIdleTimeout = 250 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	path, size, err := client.downloadFile(ctx, server.URL, filepath.Join(t.TempDir(), "file"), 4)
	if err != nil || size != 4 {
		t.Fatalf("active transfer failed: %d, %v", size, err)
	}
	if time.Since(started) <= client.downloadIdleTimeout {
		t.Fatal("fixture did not exceed the total idle-timeout duration")
	}
	assertFileContents(t, path, "xxxx")
}

func TestCancellationDuringBodyRemovesTemporaryFile(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "x")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := newQFileClient(time.Second, "https://qfile.qq.com/q/test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, _, err := client.downloadFile(ctx, server.URL, filepath.Join(root, "file"), 2)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	// Wait for a staged body byte, so the test exercises .part cleanup rather
	// than canceling before a temporary file has been created.
	deadline := time.Now().Add(5 * time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(root, ".qfile-download-*.part"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) > 0 {
			info, err := os.Stat(matches[0])
			if err == nil && info.Size() > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("response was not staged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled body read did not finish")
	}
	assertEmptyDirectory(t, root)
}

func TestCLIPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	extractLinks := func(ctx context.Context, _ string, _ time.Duration) (output, error) {
		close(started)
		<-ctx.Done()
		return output{}, ctx.Err()
	}
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() {
		done <- runCLI(ctx, []string{"https://qfile.qq.com/q/test"}, io.Discard, &stderr, extractLinks)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("command never started extraction")
	}
	cancel()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("canceled command exit code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command ignored parent cancellation")
	}
}

func assertEmptyDirectory(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected remaining files: %v, %v", entries, err)
	}
}
