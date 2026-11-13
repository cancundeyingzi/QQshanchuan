package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDownloadValidatesMetadataBeforeCommit(t *testing.T) {
	for _, test := range []struct {
		name        string
		metadata    string
		body        string
		chunked     bool
		truncated   bool
		status      int
		wantSuccess bool
		unknown     bool
	}{
		{name: "correct fixed length", metadata: "5", body: "hello", wantSuccess: true},
		{name: "correct chunked", metadata: "5", body: "hello", chunked: true, wantSuccess: true},
		{name: "short fixed length", metadata: "1000", body: "hello"},
		{name: "short chunked", metadata: "1000", body: "hello", chunked: true},
		{name: "oversized chunked", metadata: "3", body: "hello", chunked: true},
		{name: "HTTP truncated", metadata: "1000", body: "hello", truncated: true},
		{name: "empty", metadata: "0", wantSuccess: true},
		{name: "nonempty zero", metadata: "0", body: "x", chunked: true},
		{name: "missing size", body: "hello", wantSuccess: true, unknown: true},
		{name: "invalid size", metadata: "oops", body: "hello", wantSuccess: true, unknown: true},
		{name: "negative size", metadata: "-1", body: "hello", wantSuccess: true, unknown: true},
		{name: "overflow size", metadata: "999999999999999999999", body: "hello", wantSuccess: true, unknown: true},
		{name: "unexpected partial content", metadata: "5", body: "hello", status: http.StatusPartialContent},
		{name: "unexpected no content", metadata: "0", status: http.StatusNoContent},
		{name: "unexpected accepted", metadata: "5", body: "hello", status: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			existing := filepath.Join(root, "file.bin")
			if err := os.WriteFile(existing, []byte("existing"), defaultFileMode); err != nil {
				t.Fatal(err)
			}
			files := []shareFile{{Name: "file.bin", Path: "file.bin", Size: test.metadata, PhysicalID: "one"}}
			share := newDownloadTestShare(t, files, func(w http.ResponseWriter, _ *http.Request) {
				if test.truncated {
					w.Header().Set("Content-Length", "1000")
				}
				status := test.status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				if test.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, test.body)
			})
			var paths []string
			var failures int
			log := captureStdout(t, func() { paths, failures = downloadAll(context.Background(), share, root) })
			if test.wantSuccess {
				if failures != 0 || len(paths) != 1 || paths[0] == existing {
					t.Fatalf("paths = %v, failures = %d; logs: %s", paths, failures, log)
				}
				assertFileContents(t, paths[0], test.body)
			} else if failures != 1 || len(paths) != 0 {
				t.Fatalf("bad response committed: paths = %v, failures = %d; logs: %s", paths, failures, log)
			}
			assertFileContents(t, existing, "existing")
			entries, err := os.ReadDir(root)
			wantEntries := 1 + len(paths)
			if err != nil || len(entries) != wantEntries {
				t.Fatalf("unexpected artifacts: %v, %v; want %d entries", entries, err, wantEntries)
			}
			if test.unknown && !strings.Contains(log, "大小未知，未进行长度核对") {
				t.Fatalf("unknown-size success lacks qualification: %q", log)
			}
		})
	}
}

func TestDownloadPreservesEncodedFileBytes(t *testing.T) {
	data := makeGzip(t, []byte("original compressed file"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	defer server.Close()
	client, err := newQFileClient(time.Second, "https://qfile.qq.com/q/test")
	if err != nil {
		t.Fatal(err)
	}
	path, size, err := client.downloadFile(context.Background(), server.URL, filepath.Join(t.TempDir(), "file.gz"), int64(len(data)))
	if err != nil || size != int64(len(data)) {
		t.Fatalf("size = %d, error = %v", size, err)
	}
	assertFileContents(t, path, string(data))
}

func TestRejectedLengthDoesNotReserveCollisionName(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "file.txt")
	if err := os.WriteFile(destination, []byte("existing"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	_, _, err := writeNewFileValidated(destination, strings.NewReader("short"), defaultFileMode, func(size int64) error {
		return fmt.Errorf("length %d is not 1000", size)
	})
	if err == nil {
		t.Fatal("invalid staged file accepted")
	}
	path, _, err := writeNewFile(destination, strings.NewReader("good"), defaultFileMode)
	if err != nil || filepath.Base(path) != "file (1).txt" {
		t.Fatalf("collision name was consumed: %q, %v", path, err)
	}
	assertFileContents(t, destination, "existing")
}
