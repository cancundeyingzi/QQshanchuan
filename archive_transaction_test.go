package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testArchiveEntry struct {
	name string
	body string
	kind byte
}

func orderedTar(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: defaultFileMode, Typeflag: kind, Size: int64(len(entry.body))}
		if kind == tar.TypeDir {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestBrokenTarDoesNotCommitEarlierEntries(t *testing.T) {
	data := orderedTar(t, []testArchiveEntry{{name: "nested/first.txt", body: "first"}, {name: "second.txt", body: "second"}})
	for _, corruption := range []string{"header", "body"} {
		for _, format := range []string{"tar", "tar.gz", "tar.xz", "tar.zst"} {
			t.Run(corruption+"/"+format, func(t *testing.T) {
				broken := append([]byte(nil), data...)
				if corruption == "header" {
					broken[1024] ^= 0xff
				} else {
					broken = broken[:1538] // Second header declares six bytes, only two remain.
				}
				switch format {
				case "tar.gz":
					broken = makeGzip(t, broken)
				case "tar.xz":
					broken = makeXZ(t, broken)
				case "tar.zst":
					broken = makeZstd(t, broken)
				}
				root := t.TempDir()
				path := filepath.Join(root, "bundle."+format)
				if err := os.WriteFile(path, broken, defaultFileMode); err != nil {
					t.Fatal(err)
				}
				report := processVolumeArchives([]string{path}, root)
				assertFailedArchive(t, report)
				assertOnlyArchive(t, root, filepath.Base(path))
			})
		}
	}
}

func TestCompressedTarFooterMustBeValidatedBeforeCommit(t *testing.T) {
	data := orderedTar(t, []testArchiveEntry{{name: "inside.txt", body: "tentative"}})
	// Keep the decoder away from its footer when TAR reaches its end marker.
	// Simply closing a gzip reader does not check this remaining stream.
	data = append(data, make([]byte, 64<<10)...)
	for _, format := range []string{"tar.gz", "tar.xz", "tar.zst"} {
		for _, corruption := range []string{"truncated footer", "bad checksum"} {
			t.Run(format+"/"+corruption, func(t *testing.T) {
				var compressed []byte
				switch format {
				case "tar.gz":
					compressed = makeGzip(t, data)
				case "tar.xz":
					compressed = makeXZ(t, data)
				case "tar.zst":
					compressed = makeZstd(t, data)
				}
				if corruption == "truncated footer" {
					compressed = compressed[:len(compressed)-1]
				} else {
					position := len(compressed) - 1
					if format == "tar.gz" {
						position = len(compressed) - 8 // CRC32, leaving the stored size unchanged.
					}
					compressed[position] ^= 0xff
				}
				root := t.TempDir()
				path := filepath.Join(root, "bundle."+format)
				if err := os.WriteFile(path, compressed, defaultFileMode); err != nil {
					t.Fatal(err)
				}
				report := processVolumeArchives([]string{path}, root)
				assertFailedArchive(t, report)
				assertOnlyArchive(t, root, filepath.Base(path))
			})
		}
	}
}

func TestArchiveCommitFailureRollsBackFilesAndDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("original"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "existing"), defaultFolderMode); err != nil {
		t.Fatal(err)
	}
	data := orderedTar(t, []testArchiveEntry{
		{name: "keep.txt", body: "new collision"},
		{name: "empty/", kind: tar.TypeDir},
		{name: "existing/newdir/first.txt", body: "first"},
		{name: "blocked", body: "becomes a file during commit"},
		{name: "blocked/second.txt", body: "cannot be committed"},
	})
	archive := filepath.Join(root, "bundle.tar")
	if err := os.WriteFile(archive, data, defaultFileMode); err != nil {
		t.Fatal(err)
	}
	report := processVolumeArchives([]string{archive}, root)
	assertFailedArchive(t, report)
	assertFileContents(t, filepath.Join(root, "keep.txt"), "original")
	assertEmptyDirectory(t, filepath.Join(root, "existing"))
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 3 {
		t.Fatalf("rollback left artifacts or removed existing data: %v, %v", entries, err)
	}
}

func TestSuccessfulArchiveKeepsDuplicatesEmptyDirectoriesAndExistingFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("original"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	data := orderedTar(t, []testArchiveEntry{
		{name: "keep.txt", body: "first"},
		{name: "keep.txt", body: "second"},
		{name: "nested/file.txt", body: "nested"},
		{name: "empty/", kind: tar.TypeDir},
	})
	files, err := extractTar(bytes.NewReader(data), root, root)
	if err != nil || len(files) != 3 {
		t.Fatalf("files = %v, error = %v", files, err)
	}
	assertFileContents(t, filepath.Join(root, "keep.txt"), "original")
	assertFileContents(t, filepath.Join(root, "keep (1).txt"), "first")
	assertFileContents(t, filepath.Join(root, "keep (2).txt"), "second")
	assertFileContents(t, filepath.Join(root, "nested", "file.txt"), "nested")
	assertEmptyDirectory(t, filepath.Join(root, "empty"))
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 5 {
		t.Fatalf("unexpected archive artifacts: %v, %v", entries, err)
	}
}

func TestUnsafeLaterArchiveEntryLeavesNoEarlierOutput(t *testing.T) {
	root := t.TempDir()
	data := orderedTar(t, []testArchiveEntry{{name: "nested/first.txt", body: "first"}, {name: "../outside.txt", body: "unsafe"}})
	files, err := extractTar(bytes.NewReader(data), root, root)
	if err == nil || len(files) != 0 {
		t.Fatalf("unsafe archive accepted: %v, %v", files, err)
	}
	assertEmptyDirectory(t, root)
}

func TestArchiveCommitRechecksDirectoryLinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	stage, err := os.MkdirTemp(root, ".qfile-extract-*")
	if err != nil {
		t.Fatal(err)
	}
	stagedPath := filepath.Join(stage, "file")
	if err := os.WriteFile(stagedPath, []byte("must not escape"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	transaction := archiveTransaction{root: root, destination: root, stage: stage, entries: []stagedArchiveEntry{{name: "linked/file.txt", path: stagedPath}}}
	makeDirectoryLink(t, outside, filepath.Join(root, "linked"))
	files, err := transaction.commit()
	if err == nil || len(files) != 0 {
		t.Fatalf("link at commit accepted: %v, %v", files, err)
	}
	assertEmptyDirectory(t, outside)
}

func TestArchiveRollbackReportsAndPreservesNonemptyDirectory(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, defaultFolderMode); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(owned, "other.txt")
	if err := os.WriteFile(otherFile, []byte("preserve"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	transaction := archiveTransaction{root: root, directories: []string{owned}}
	err := transaction.rollback()
	if err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatalf("missing rollback failure path: %v", err)
	}
	assertFileContents(t, otherFile, "preserve")
}

func TestArchiveRollbackRefusesReplacedDirectoryLink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	outsideFile := filepath.Join(outside, "file.txt")
	if err := os.WriteFile(outsideFile, []byte("preserve outside"), defaultFileMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	makeDirectoryLink(t, outside, link)
	transaction := archiveTransaction{root: root, created: []string{filepath.Join(link, "file.txt")}, directories: []string{link}}
	if err := transaction.rollback(); err == nil || !strings.Contains(err.Error(), "unsafe rollback path") {
		t.Fatalf("replaced path was not rejected: %v", err)
	}
	assertFileContents(t, outsideFile, "preserve outside")
}

func TestArchiveCannotCommitIntoItsStagingDirectory(t *testing.T) {
	root := t.TempDir()
	stage, err := os.MkdirTemp(root, ".qfile-extract-*")
	if err != nil {
		t.Fatal(err)
	}
	transaction := archiveTransaction{root: root, destination: root, stage: stage}
	data := orderedTar(t, []testArchiveEntry{{name: filepath.Base(stage) + "/file.txt", body: "would be removed by cleanup"}})
	if err := transaction.extract(bytes.NewReader(data)); err == nil {
		t.Fatal("archive was allowed to target its own temporary storage")
	}
	assertEmptyDirectory(t, stage)
}

func assertFailedArchive(t *testing.T, report archiveReport) {
	t.Helper()
	if report.found != 1 || report.failures != 1 || report.completed != 0 || report.extracted != 0 || len(report.files) != 0 {
		t.Fatalf("partial archive counted as success: %+v", report)
	}
}

func assertOnlyArchive(t *testing.T, root, name string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("partial extraction artifacts: %v, %v", entries, err)
	}
}
