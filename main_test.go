package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func TestProcessVolumeArchivesTar(t *testing.T) {
	root := t.TempDir()
	tarData := makeTar(t, map[string]string{
		"nested/one.txt": "first file",
		"two.txt":        "second file",
	})
	parts := writeSplit(t, filepath.Join(root, "bundle.tar"), tarData)

	report := processVolumeArchives(parts, root)
	if report.found != 1 || report.completed != 1 || report.extracted != 2 || report.failures != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	assertFileContents(t, filepath.Join(root, "nested", "one.txt"), "first file")
	assertFileContents(t, filepath.Join(root, "two.txt"), "second file")
}

func TestProcessVolumeArchivesCompressed(t *testing.T) {
	tests := []struct {
		name string
		file string
		make func(*testing.T, []byte) []byte
	}{
		{
			name: "gzip",
			file: "message.gz",
			make: makeGzip,
		},
		{
			name: "xz",
			file: "message.xz",
			make: makeXZ,
		},
		{
			name: "zstd",
			file: "message.zst",
			make: makeZstd,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			content := []byte("correctly merged content")
			parts := writeSplit(t, filepath.Join(root, test.file), test.make(t, content))

			report := processVolumeArchives(parts, root)
			if report.found != 1 || report.completed != 1 || report.extracted != 1 || report.failures != 0 {
				t.Fatalf("unexpected report: %+v", report)
			}
			assertFileContents(t, filepath.Join(root, "message"), string(content))
		})
	}
}

func TestProcessSingleArchives(t *testing.T) {
	const message = "single archive content"
	tests := []struct {
		name      string
		filename  string
		make      func(*testing.T) []byte
		output    string
		contents  string
		extracted int
	}{
		{
			name:     "tar",
			filename: "bundle.tar",
			make: func(t *testing.T) []byte {
				return makeTar(t, map[string]string{"inside.txt": message})
			},
			output:    "inside.txt",
			contents:  message,
			extracted: 1,
		},
		{
			name:     "gzip",
			filename: "message.gz",
			make: func(t *testing.T) []byte {
				return makeGzip(t, []byte(message))
			},
			output:    "message",
			contents:  message,
			extracted: 1,
		},
		{
			name:     "xz",
			filename: "message.xz",
			make: func(t *testing.T) []byte {
				return makeXZ(t, []byte(message))
			},
			output:    "message",
			contents:  message,
			extracted: 1,
		},
		{
			name:     "zstd",
			filename: "message.zstd",
			make: func(t *testing.T) []byte {
				return makeZstd(t, []byte(message))
			},
			output:    "message",
			contents:  message,
			extracted: 1,
		},
		{
			name:     "tar-zstd",
			filename: "bundle.tar.zst",
			make: func(t *testing.T) []byte {
				return makeZstd(t, makeTar(t, map[string]string{"inside.txt": message}))
			},
			output:    "inside.txt",
			contents:  message,
			extracted: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, test.filename)
			if err := os.WriteFile(archivePath, test.make(t), defaultFileMode); err != nil {
				t.Fatal(err)
			}

			report := processVolumeArchives([]string{archivePath}, root)
			if report.found != 1 || report.completed != 1 || report.extracted != test.extracted || report.failures != 0 {
				t.Fatalf("unexpected report: %+v", report)
			}
			assertFileContents(t, filepath.Join(root, test.output), test.contents)
		})
	}
}

func TestSafeLocalAndArchivePathsStayWithinRoot(t *testing.T) {
	root := t.TempDir()
	destination, err := localDownloadPath(root, "folder/a:b?.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "folder", "a_b_.txt")
	if destination != want {
		t.Fatalf("destination = %q, want %q", destination, want)
	}

	directory := filepath.Join(root, "extract")
	if err := os.MkdirAll(directory, defaultFolderMode); err != nil {
		t.Fatal(err)
	}
	if _, err := safeArchivePath(directory, root, "../outside.txt"); err == nil {
		t.Fatal("path traversal archive entry was accepted")
	}
	if target, err := safeArchivePath(directory, root, "inside.txt"); err != nil || target != filepath.Join(directory, "inside.txt") {
		t.Fatalf("safe archive path = %q, %v", target, err)
	}
}

func TestArchiveExtractionRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	makeDirectoryLink(t, outside, filepath.Join(root, "linked"))
	archivePath := filepath.Join(root, "bundle.tar")
	if err := os.WriteFile(archivePath, makeTar(t, map[string]string{"linked/escaped.txt": "must stay inside"}), defaultFileMode); err != nil {
		t.Fatal(err)
	}

	report := processVolumeArchives([]string{archivePath}, root)
	if report.completed != 0 || report.failures != 1 || len(report.files) != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive wrote through directory symlink: %v", err)
	}
}

func TestLocalDownloadPathRejectsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	makeDirectoryLink(t, outside, filepath.Join(root, "linked"))
	if _, err := localDownloadPath(root, "linked/escaped.txt"); err == nil {
		t.Fatal("download path through a directory symlink was accepted")
	}
}

func TestVolumeSequenceValidation(t *testing.T) {
	tests := []struct {
		name    string
		parts   []string
		wantErr bool
	}{
		{name: "base plus second volume", parts: []string{"bundle.tar", "bundle.tar.002"}},
		{name: "numbered volumes", parts: []string{"bundle.tar.001", "bundle.tar.002", "bundle.tar.003"}},
		{name: "gap after base", parts: []string{"bundle.tar", "bundle.tar.003"}, wantErr: true},
		{name: "gap in numbered volumes", parts: []string{"bundle.tar.001", "bundle.tar.003"}, wantErr: true},
		{name: "numbered volumes must start at one", parts: []string{"bundle.tar.002", "bundle.tar.003"}, wantErr: true},
		{name: "base successor must start at two", parts: []string{"bundle.tar", "bundle.tar.001"}, wantErr: true},
		{name: "duplicate numeric volume", parts: []string{"bundle.tar.001", "bundle.tar.0001", "bundle.tar.002"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			group := volumeGroup{basePath: "bundle.tar", parts: test.parts, kind: archiveTar}
			err := validateVolumeSequence(group)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateVolumeSequence() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestMissingVolumeDoesNotExtractPartialArchive(t *testing.T) {
	root := t.TempDir()
	tarData := makeTar(t, map[string]string{"inside.txt": "content"})
	firstPart := filepath.Join(root, "bundle.tar.001")
	thirdPart := filepath.Join(root, "bundle.tar.003")
	if err := os.WriteFile(firstPart, tarData[:len(tarData)/2], defaultFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thirdPart, tarData[len(tarData)/2:], defaultFileMode); err != nil {
		t.Fatal(err)
	}

	report := processVolumeArchives([]string{firstPart, thirdPart}, root)
	if report.completed != 0 || report.failures != 1 || report.extracted != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(root, "inside.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial archive was extracted: %v", err)
	}
}

func TestDownloadFileUsesMetadataDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="wrong-name.bin"`)
		_, _ = writer.Write([]byte("downloaded bytes"))
	}))
	defer server.Close()

	client, err := newQFileClient(time.Second, "https://qfile.qq.com/q/example")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "正确文件名.txt")
	savedPath, size, err := client.downloadFile(context.Background(), server.URL, destination)
	if err != nil {
		t.Fatal(err)
	}
	if savedPath != destination || size != int64(len("downloaded bytes")) {
		t.Fatalf("saved = %q, %d", savedPath, size)
	}
	assertFileContents(t, destination, "downloaded bytes")
}

func TestCalculateFileHashes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checksum.txt")
	if err := os.WriteFile(path, []byte("abc"), defaultFileMode); err != nil {
		t.Fatal(err)
	}

	values, err := calculateFileHashes(path)
	if err != nil {
		t.Fatal(err)
	}
	if values.size != 3 {
		t.Fatalf("size = %d, want 3", values.size)
	}
	if values.md5 != "900150983cd24fb0d6963f7d28e17f72" {
		t.Fatalf("MD5 = %s", values.md5)
	}
	if values.sha1 != "a9993e364706816aba3e25717850c26c9cd0d89d" {
		t.Fatalf("SHA-1 = %s", values.sha1)
	}
	if values.sha256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("SHA-256 = %s", values.sha256)
	}
}

func makeTar(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, content := range entries {
		header := &tar.Header{Name: name, Mode: defaultFileMode, Size: int64(len(content))}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeXZ(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := xz.NewWriter(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func makeZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := zstd.NewWriter(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeSplit(t *testing.T, basePath string, data []byte) []string {
	t.Helper()
	splitAt := len(data) / 2
	if splitAt == 0 {
		splitAt = 1
	}
	if err := os.WriteFile(basePath, data[:splitAt], defaultFileMode); err != nil {
		t.Fatal(err)
	}
	secondPart := basePath + ".002"
	if err := os.WriteFile(secondPart, data[splitAt:], defaultFileMode); err != nil {
		t.Fatal(err)
	}
	return []string{basePath, secondPart}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != want {
		t.Fatalf("contents of %q = %q, want %q", path, got, want)
	}
}

func makeDirectoryLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	} else if runtime.GOOS != "windows" {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	// Directory junctions do not require Windows' symbolic-link privilege and
	// exercise the same unsafe path traversal on standard development machines.
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("directory links are unavailable: %v: %s", err, output)
	}
}
