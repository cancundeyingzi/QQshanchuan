package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTextOutputHasTwoReversibleFieldsPerFile(t *testing.T) {
	result := output{Files: []outputFile{
		{Path: "普通/文件.txt", DownloadURL: "https://example.com/file?a=1&b=2"},
		{Path: "tab\tcr\rlf\n\\literal\\n\".txt", DownloadURL: "https://example.com/\t\r\n\x1b[31m"},
		{Path: "坏\x7f\u0085\u2028\u202e\xff.txt", Error: "失败\t换列\n伪造记录\x1b]0;标题\a"},
	}}
	var buffer bytes.Buffer
	if err := writeText(&buffer, result); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buffer.String(), "\n"), "\n")
	if len(lines) != len(result.Files) {
		t.Fatalf("record count = %d, want %d: %q", len(lines), len(result.Files), buffer.String())
	}
	if lines[0] != "普通/文件.txt\thttps://example.com/file?a=1&b=2" {
		t.Fatalf("ordinary fields changed: %q", lines[0])
	}
	for index, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || strings.ContainsAny(line, "\r\x1b\x7f\u0085\u2028\u202e") {
			t.Fatalf("unsafe record: %q", line)
		}
		want := result.Files[index].DownloadURL
		if result.Files[index].Error != "" {
			want = "ERROR: " + result.Files[index].Error
		}
		for column, expected := range []string{result.Files[index].Path, want} {
			// Quotes are literal field characters; other escapes follow Go's
			// string-literal rules. This decoder is independent of the encoder.
			quoted := `"` + strings.ReplaceAll(fields[column], `"`, `\"`) + `"`
			decoded, err := strconv.Unquote(quoted)
			if err != nil || decoded != expected {
				t.Fatalf("record %d field %d round trip = %q, %v; want %q", index, column, decoded, err, expected)
			}
		}
	}
}

type failingOutput struct {
	writes   int
	closes   int
	writeErr error
	closeErr error
	short    bool
}

func (writer *failingOutput) Write(data []byte) (int, error) {
	writer.writes++
	if writer.writeErr != nil {
		return 0, writer.writeErr
	}
	if writer.short {
		return len(data) - 1, nil
	}
	return len(data), nil
}

func (writer *failingOutput) Close() error {
	writer.closes++
	return writer.closeErr
}

func TestOutputPropagatesWriteShortWriteAndCloseErrors(t *testing.T) {
	writeErr, closeErr := errors.New("disk full"), errors.New("close failed")
	result := output{Files: []outputFile{{Path: "one", DownloadURL: "url"}, {Path: "two", DownloadURL: "url"}}}
	for _, asText := range []bool{true, false} {
		for _, test := range []struct {
			name   string
			writer failingOutput
			want   []error
		}{
			{name: "write", writer: failingOutput{writeErr: writeErr}, want: []error{writeErr}},
			{name: "short", writer: failingOutput{short: true}, want: []error{io.ErrShortWrite}},
			{name: "close", writer: failingOutput{closeErr: closeErr}, want: []error{closeErr}},
			{name: "both", writer: failingOutput{writeErr: writeErr, closeErr: closeErr}, want: []error{writeErr, closeErr}},
		} {
			t.Run(strconv.FormatBool(asText)+"/"+test.name, func(t *testing.T) {
				writer := test.writer
				err := writeOutput(&writer, &writer, result, asText)
				for _, want := range test.want {
					if !errors.Is(err, want) {
						t.Fatalf("error = %v, want %v", err, want)
					}
				}
				if writer.closes != 1 {
					t.Fatalf("close count = %d", writer.closes)
				}
				if test.name != "close" && writer.writes != 1 {
					t.Fatalf("continued after a failed write: %d writes", writer.writes)
				}
			})
		}
	}
}

func TestCLIReportsOutputFailureWithNonzeroExit(t *testing.T) {
	extractLinks := func(context.Context, string, time.Duration) (output, error) {
		return output{Files: []outputFile{{Path: "file.txt", DownloadURL: "url"}}}, nil
	}
	for _, format := range []string{"text", "json"} {
		for _, short := range []bool{true, false} {
			t.Run(format+"/short="+strconv.FormatBool(short), func(t *testing.T) {
				writer := &failingOutput{short: short}
				if !short {
					writer.writeErr = errors.New("broken pipe\n\x1b[31m")
				}
				args := []string{"https://qfile.qq.com/q/test"}
				if format == "text" {
					args = append([]string{"-text"}, args...)
				}
				var stderr bytes.Buffer
				if code := runCLI(context.Background(), args, writer, &stderr, extractLinks); code != 1 {
					t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
				}
				if strings.ContainsRune(stderr.String(), '\x1b') || strings.Count(stderr.String(), "\n") != 1 {
					t.Fatalf("unsafe stderr: %q", stderr.String())
				}
			})
		}
	}
	path := filepath.Join(t.TempDir(), "links.txt")
	var stderr bytes.Buffer
	if code := runCLI(context.Background(), []string{"-text", "-output", path, "https://qfile.qq.com/q/test"}, io.Discard, &stderr, extractLinks); code != 0 {
		t.Fatalf("file output exit code = %d, stderr = %q", code, stderr.String())
	}
	assertFileContents(t, path, "file.txt\turl\n")
}

func TestCLIRejectsUnboundedTimeouts(t *testing.T) {
	for _, args := range [][]string{{"-timeout=0"}, {"-timeout=-1s"}, {"-download-idle-timeout=0"}, {"-download-idle-timeout=-1s"}} {
		var stderr bytes.Buffer
		if code := runCLI(context.Background(), args, io.Discard, &stderr, nil); code != 2 {
			t.Fatalf("args %v: exit code = %d, stderr = %q", args, code, stderr.String())
		}
	}
}

func TestDownloadLogsEscapeRemotePathAndError(t *testing.T) {
	text := captureStdout(t, func() {
		share := &parsedShare{Files: []shareFile{{Path: "中文\n伪造\r\t\x1b[2J\u202e", Error: "错误\x1b]0;标题\a\u009b31m"}}}
		paths, failures := downloadAll(context.Background(), share, t.TempDir())
		if failures != 1 || len(paths) != 0 {
			t.Fatalf("paths = %v, failures = %d", paths, failures)
		}
	})
	if strings.Count(text, "\n") != 1 || strings.ContainsAny(text, "\r\t\x1b\a\u009b\u202e") {
		t.Fatalf("unsafe log: %q", text)
	}
	if !strings.Contains(text, `中文\n伪造\r\t\x1b[2J\u202e`) || !strings.Contains(text, `错误\x1b]0;标题\x07\u009b31m`) {
		t.Fatalf("missing readable escaped content: %q", text)
	}
	if printablePath("") != "/" || terminalText(`C:\中文\file.txt`) != `C:\中文\file.txt` {
		t.Fatal("ordinary log paths changed")
	}
}

func TestPromptEscapesErrorsAndReturnsCancellation(t *testing.T) {
	var buffer bytes.Buffer
	parse := func(context.Context, string, time.Duration) (*parsedShare, error) {
		return nil, errors.New("remote\nforged\x1b]0;title\a")
	}
	_, err := promptForShare(context.Background(), bufio.NewReader(strings.NewReader("qfile.qq.com/q/test\n\n")), &buffer, time.Second, parse)
	if err != nil || strings.ContainsAny(buffer.String(), "\x1b\a") || strings.Contains(buffer.String(), "\nforged") {
		t.Fatalf("prompt output = %q, error = %v", buffer.String(), err)
	}
	parse = func(context.Context, string, time.Duration) (*parsedShare, error) { return nil, context.Canceled }
	buffer.Reset()
	_, err = promptForShare(context.Background(), bufio.NewReader(strings.NewReader("qfile.qq.com/q/test\n")), &buffer, time.Second, parse)
	if !errors.Is(err, context.Canceled) || strings.Contains(buffer.String(), "重新输入") {
		t.Fatalf("canceled parsing retried: %q, %v", buffer.String(), err)
	}
}

func captureStdout(t *testing.T, action func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	defer func() {
		os.Stdout = previous
		_ = writer.Close()
		_ = reader.Close()
	}()
	action()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
