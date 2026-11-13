package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPromptForShareRetries(t *testing.T) {
	want := &parsedShare{ShareURL: "https://qfile.qq.com/q/good"}
	var parsed []string
	parse := func(_ context.Context, raw string, _ time.Duration) (*parsedShare, error) {
		parsed = append(parsed, raw)
		if strings.HasSuffix(raw, "/expired") {
			return nil, errors.New("分享已失效")
		}
		return want, nil
	}
	reader := bufio.NewReader(strings.NewReader("错误链接\nhttps://qfile.qq.com/q/expired\nhttps://qfile.qq.com/q/good\n\n"))
	var output bytes.Buffer
	got, err := promptForShare(context.Background(), reader, &output, time.Second, parse)
	if err != nil || got != want {
		t.Fatalf("share = %v, error = %v", got, err)
	}
	if len(parsed) != 2 || parsed[1] != want.ShareURL {
		t.Fatalf("parsed URLs = %v", parsed)
	}
	if strings.Count(output.String(), "重新输入") != 2 || !strings.Contains(output.String(), "分享已失效") {
		t.Fatalf("missing retry/error messages: %s", &output)
	}
	// The retry loop must leave the following Enter for the download prompt.
	if line, err := reader.ReadString('\n'); err != nil || line != "\n" {
		t.Fatalf("following input = %q, %v", line, err)
	}
}

func TestPromptForShareEOFAndEmptyInput(t *testing.T) {
	for _, input := range []string{"", "\n", " \r\n", "错误链接", "https://qfile.qq.com/q/expired"} {
		t.Run(strconv.Quote(input), func(t *testing.T) {
			calls := 0
			parse := func(context.Context, string, time.Duration) (*parsedShare, error) {
				calls++
				return nil, errors.New("解析失败")
			}
			var output bytes.Buffer
			got, err := promptForShare(context.Background(), bufio.NewReader(strings.NewReader(input)), &output, time.Second, parse)
			if err != nil || got != nil || calls > 1 {
				t.Fatalf("share = %v, error = %v, parse calls = %d", got, err, calls)
			}
			if strings.Count(output.String(), "请粘贴 QQ 闪传链接，然后按 Enter") != 1 {
				t.Fatalf("unexpected repeated prompt at EOF: %s", &output)
			}
		})
	}
	t.Run("valid final line without newline", func(t *testing.T) {
		want := &parsedShare{}
		parse := func(context.Context, string, time.Duration) (*parsedShare, error) { return want, nil }
		got, err := promptForShare(context.Background(), bufio.NewReader(strings.NewReader("qfile.qq.com/q/good")), io.Discard, time.Second, parse)
		if err != nil || got != want {
			t.Fatalf("share = %v, error = %v", got, err)
		}
	})
}

type downloadOutcome struct {
	paths    []string
	failures int
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Both API calls and file transfers go to a local server. Integration tests
// supply real payload sizes; scheduling tests use a separate download stub.
func newDownloadTestShare(t *testing.T, files []shareFile, handler http.HandlerFunc) *parsedShare {
	t.Helper()
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" {
			handler(w, r)
			return
		}
		var request batchDownloadRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response := batchDownloadResponse{}
		for _, info := range request.DownloadInfo {
			response.DownloadResponses = append(response.DownloadResponses, batchDownloadResult{
				BatchID: info.BatchID, RetCode: "0", URL: baseURL + "/file/" + info.BatchID,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"retcode": 0, "data": response})
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)
	localURL, err := url.Parse(server.URL + "/rpc")
	if err != nil {
		t.Fatal(err)
	}
	client, err := newQFileClient(5*time.Second, "https://qfile.qq.com/q/test")
	if err != nil {
		t.Fatal(err)
	}
	client.httpClient.Transport = testRoundTripper(func(req *http.Request) (*http.Response, error) {
		local := req.Clone(req.Context())
		local.URL = localURL
		local.Host = localURL.Host
		return server.Client().Transport.RoundTrip(local)
	})
	client.downloadClient = server.Client()
	return &parsedShare{Client: client, Files: files}
}

func testDownloadFiles(sizes []string) []shareFile {
	files := make([]shareFile, len(sizes))
	for index, size := range sizes {
		name := fmt.Sprintf("file-%02d.txt", index)
		files[index] = shareFile{Name: name, Path: name, Size: size, PhysicalID: strconv.Itoa(index)}
	}
	return files
}

func awaitDownloadStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case id := <-started:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a download to start")
		return ""
	}
}

func awaitDownloads(t *testing.T, done <-chan downloadOutcome) downloadOutcome {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for downloads to finish")
		return downloadOutcome{}
	}
}

// Scheduler tests control completion independently of HTTP and metadata checks.
func schedulingDownload(started chan<- string, release <-chan struct{}) shareDownloader {
	return func(ctx context.Context, _ *qfileClient, file shareFile, root string) (string, int64, error) {
		started <- file.PhysicalID
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-release:
			return writeNewFile(filepath.Join(root, file.Path), strings.NewReader("/file/"+file.PhysicalID), defaultFileMode)
		}
	}
}

func TestDownloadAllConcurrencyAndRefill(t *testing.T) {
	for _, test := range []struct {
		name  string
		size  string
		limit int
	}{
		{"10 MB", strconv.FormatInt(10<<20, 10), 10},
		{"20 MB boundary", strconv.FormatInt(20<<20, 10), 10},
		{"above 20 MB", strconv.FormatInt((20<<20)+1, 10), 5},
		{"100 MB", strconv.FormatInt(100<<20, 10), 5},
		{"200 MB boundary", strconv.FormatInt(200<<20, 10), 5},
		{"above 200 MB", strconv.FormatInt((200<<20)+1, 10), 2},
		{"500 MB", strconv.FormatInt(500<<20, 10), 2},
		{"1 GB boundary", strconv.FormatInt(1<<30, 10), 2},
		{"above 1 GB", strconv.FormatInt((1<<30)+1, 10), 1},
		{"unknown", "", 1},
		{"invalid", "invalid", 1},
		{"negative", "-1", 1},
		{"overflow", "9999999999999999999999999999", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			root := t.TempDir()
			sizes := make([]string, test.limit+2)
			for index := range sizes {
				sizes[index] = test.size
			}
			files := testDownloadFiles(sizes)
			started := make(chan string, len(files))
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			share := &parsedShare{Files: files}
			done := make(chan downloadOutcome, 1)
			go func() {
				paths, failures := downloadAllWith(ctx, share, root, schedulingDownload(started, release))
				done <- downloadOutcome{paths, failures}
			}()
			for range test.limit {
				awaitDownloadStart(t, started)
			}
			select {
			case id := <-started:
				t.Fatalf("download %s exceeded concurrency limit %d", id, test.limit)
			case <-time.After(75 * time.Millisecond):
			}
			// Completing one task must refill its slot before the others finish.
			release <- struct{}{}
			awaitDownloadStart(t, started)
			releaseOnce.Do(func() { close(release) })
			result := awaitDownloads(t, done)
			if result.failures != 0 || len(result.paths) != len(files) {
				t.Fatalf("result = %+v", result)
			}
			for index, path := range result.paths {
				if path != filepath.Join(root, files[index].Path) {
					t.Fatalf("result order changed: %v", result.paths)
				}
				assertFileContents(t, path, "/file/"+files[index].PhysicalID)
			}
		})
	}
}

func TestDownloadAllMixedSizesShareBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	files := testDownloadFiles([]string{
		strconv.FormatInt(500<<20, 10), // 5 slots
		strconv.FormatInt(100<<20, 10), // 2 slots
		strconv.FormatInt(100<<20, 10), // 2 slots
		strconv.FormatInt(10<<20, 10),  // 1 slot
		strconv.FormatInt(10<<20, 10),  // must wait
	})
	started := make(chan string, len(files))
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	share := &parsedShare{Files: files}
	done := make(chan downloadOutcome, 1)
	go func() {
		paths, failures := downloadAllWith(ctx, share, root, schedulingDownload(started, release))
		done <- downloadOutcome{paths, failures}
	}()
	for range 4 {
		awaitDownloadStart(t, started)
	}
	select {
	case id := <-started:
		t.Fatalf("mixed-size budget exceeded by %s", id)
	case <-time.After(75 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	result := awaitDownloads(t, done)
	if result.failures != 0 || len(result.paths) != len(files) {
		t.Fatalf("result = %+v", result)
	}
}

func TestDownloadAllFailuresAndNameCollisions(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "a_b.txt")
	if err := os.WriteFile(destination, []byte("existing"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	files := []shareFile{
		{Name: "a:b.txt", Path: "a:b.txt", Size: "3", PhysicalID: "one"},
		{Name: "a?b.txt", Path: "a?b.txt", Size: "3", PhysicalID: "two"},
		{Name: "a_b.txt", Path: "a_b.txt", Size: "5", PhysicalID: "three"},
		{Name: "bad.txt", Path: "bad.txt", Size: "10", PhysicalID: "bad"},
		{Name: "skip.txt", Path: "skip.txt", Error: "unavailable"},
	}
	share := newDownloadTestShare(t, files, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			http.Error(w, "download failed", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, strings.TrimPrefix(r.URL.Path, "/file/"))
	})
	paths, failures := downloadAll(context.Background(), share, root)
	if failures != 2 || len(paths) != 3 {
		t.Fatalf("paths = %v, failures = %d", paths, failures)
	}
	seen := make(map[string]bool)
	for index, path := range paths {
		if path == destination || seen[path] {
			t.Fatalf("overwritten or shared destination: %v", paths)
		}
		seen[path] = true
		assertFileContents(t, path, files[index].PhysicalID)
	}
	assertFileContents(t, destination, "existing")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 4 {
		t.Fatalf("unexpected leftovers: %v, %v", entries, err)
	}
}

func TestDownloadAllCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	files := testDownloadFiles([]string{"", "", ""})
	started := make(chan string, len(files))
	share := newDownloadTestShare(t, files, func(_ http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-r.Context().Done()
	})
	done := make(chan downloadOutcome, 1)
	go func() {
		paths, failures := downloadAll(ctx, share, root)
		done <- downloadOutcome{paths, failures}
	}()
	awaitDownloadStart(t, started)
	cancel()
	result := awaitDownloads(t, done)
	if result.failures != len(files) || len(result.paths) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(started) != 0 {
		t.Fatal("queued downloads started after cancellation")
	}
}

func TestWriteNewFileConcurrentCollisions(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "same.txt")
	if err := os.WriteFile(destination, []byte("existing"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	const count = 20
	type result struct {
		path    string
		content string
		err     error
	}
	results := make(chan result, count)
	for index := range count {
		go func() {
			content := fmt.Sprintf("content-%d", index)
			path, _, err := writeNewFile(destination, strings.NewReader(content), defaultFileMode)
			results <- result{path, content, err}
		}()
	}
	seen := make(map[string]bool)
	for range count {
		got := <-results
		if got.err != nil || got.path == destination || seen[got.path] {
			t.Fatalf("unexpected save result: %+v", got)
		}
		seen[got.path] = true
		assertFileContents(t, got.path, got.content)
	}
	assertFileContents(t, destination, "existing")
}
