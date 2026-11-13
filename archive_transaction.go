package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

type stagedArchiveEntry struct {
	name string
	path string // Empty for an explicit directory entry.
}

type archiveTransaction struct {
	destination string
	root        string
	stage       string
	entries     []stagedArchiveEntry
	created     []string
	directories []string
}

func extractTarTransaction(reader io.Reader, destination, root string) (files []string, err error) {
	if err := rejectSymlinkComponents(root, destination); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(destination, ".qfile-extract-*")
	if err != nil {
		return nil, fmt.Errorf("create archive staging directory: %w", err)
	}
	transaction := &archiveTransaction{destination: destination, root: root, stage: stage}
	defer func() {
		if cleanupErr := os.RemoveAll(stage); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove archive staging directory %q: %w", stage, cleanupErr))
		}
		if err != nil {
			err = errors.Join(err, transaction.rollback())
			files = nil
		}
	}()
	if err = transaction.extract(reader); err != nil {
		return nil, err
	}
	// TAR's end marker can precede the compressed stream's checksum/footer.
	// Nothing is committed until the underlying gzip/xz/zstd reader reaches EOF.
	if _, err = io.Copy(io.Discard, reader); err != nil {
		return nil, fmt.Errorf("finish archive stream: %w", err)
	}
	return transaction.commit()
}

func (transaction *archiveTransaction) extract(reader io.Reader) error {
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar stream: %w", err)
		}
		if header.Name == "" {
			continue
		}
		if _, err := transaction.target(header.Name); err != nil {
			return fmt.Errorf("unsafe archive entry %q: %w", header.Name, err)
		}
		entry := stagedArchiveEntry{name: header.Name}
		switch header.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg, tar.TypeRegA:
			// Flat internal names prevent entries from affecting one another's
			// staging paths, including duplicate names and file/dir conflicts.
			path := filepath.Join(transaction.stage, strconv.Itoa(len(transaction.entries)))
			saved, _, err := writeNewFile(path, tarReader, defaultFileMode)
			if err != nil {
				return fmt.Errorf("extract %q: %w", header.Name, err)
			}
			entry.path = saved
		default:
			logf("  跳过非普通归档项：%s\n", header.Name)
			continue
		}
		transaction.entries = append(transaction.entries, entry)
	}
}

func (transaction *archiveTransaction) commit() ([]string, error) {
	fileCommitMu.Lock()
	defer fileCommitMu.Unlock()
	for _, entry := range transaction.entries {
		target, err := transaction.target(entry.name)
		if err != nil {
			return nil, fmt.Errorf("unsafe archive entry %q at commit: %w", entry.name, err)
		}
		directory := filepath.Dir(target)
		if entry.path == "" {
			directory = target
		}
		if err := transaction.mkdir(directory); err != nil {
			return nil, fmt.Errorf("create archive directory %q: %w", directory, err)
		}
		if entry.path == "" {
			continue
		}
		target, err = chooseAvailablePath(target)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(entry.path, target); err != nil {
			return nil, fmt.Errorf("commit archive entry %q: %w", entry.name, err)
		}
		transaction.created = append(transaction.created, target)
	}
	return append([]string(nil), transaction.created...), nil
}

func (transaction *archiveTransaction) target(name string) (string, error) {
	target, err := safeArchivePath(transaction.destination, transaction.root, name)
	if err != nil {
		return "", err
	}
	if pathWithin(transaction.stage, target) {
		return "", errors.New("entry targets the archive staging directory")
	}
	return target, nil
}

func (transaction *archiveTransaction) mkdir(directory string) error {
	missing := make([]string, 0)
	for current := directory; ; current = filepath.Dir(current) {
		if err := rejectSymlinkComponents(transaction.root, current); err != nil {
			return err
		}
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
	}
	for index := len(missing) - 1; index >= 0; index-- {
		path := missing[index]
		if err := os.Mkdir(path, defaultFolderMode); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
			if err := rejectSymlinkComponents(transaction.root, path); err != nil {
				return err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", path)
			}
			continue
		}
		transaction.directories = append(transaction.directories, path)
	}
	return nil
}

func (transaction *archiveTransaction) rollback() error {
	fileCommitMu.Lock()
	defer fileCommitMu.Unlock()
	var failures []error
	for index := len(transaction.created) - 1; index >= 0; index-- {
		path := transaction.created[index]
		if err := rejectSymlinkComponents(transaction.root, path); err != nil {
			failures = append(failures, fmt.Errorf("unsafe rollback path %q: %w", path, err))
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("rollback archive file %q: %w", path, err))
		}
	}
	for index := len(transaction.directories) - 1; index >= 0; index-- {
		path := transaction.directories[index]
		if err := rejectSymlinkComponents(transaction.root, path); err != nil {
			failures = append(failures, fmt.Errorf("unsafe rollback path %q: %w", path, err))
			continue
		}
		// Remove only directories created by this transaction, and only if empty.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("rollback archive directory %q: %w", path, err))
		}
	}
	return errors.Join(failures...)
}
