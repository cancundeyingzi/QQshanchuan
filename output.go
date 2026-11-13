package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Text fields escape backslashes as well as non-printable characters, making
// the two-column format reversible even when a name contains literal "\\n".
func escapeTextField(value string) string { return escapeDisplayText(value, true) }

// Log messages keep ordinary Windows paths readable. Only characters that can
// change terminal state, line boundaries, or visual ordering need escaping.
func terminalText(value string) string { return escapeDisplayText(value, false) }

func escapeDisplayText(value string, escapeBackslash bool) string {
	var builder strings.Builder
	for len(value) > 0 {
		character, size := utf8.DecodeRuneInString(value)
		if character == utf8.RuneError && size == 1 {
			fmt.Fprintf(&builder, `\x%02x`, value[0])
			value = value[1:]
			continue
		}
		value = value[size:]
		switch character {
		case '\\':
			if escapeBackslash {
				builder.WriteString(`\\`)
			} else {
				builder.WriteRune(character)
			}
		case '\t':
			builder.WriteString(`\t`)
		case '\r':
			builder.WriteString(`\r`)
		case '\n':
			builder.WriteString(`\n`)
		default:
			if unicode.IsPrint(character) {
				builder.WriteRune(character)
			} else if character <= 0x7f {
				fmt.Fprintf(&builder, `\x%02x`, character)
			} else if character <= 0xffff {
				fmt.Fprintf(&builder, `\u%04x`, character)
			} else {
				fmt.Fprintf(&builder, `\U%08x`, character)
			}
		}
	}
	return builder.String()
}

func logf(format string, args ...any) {
	for index, arg := range args {
		switch value := arg.(type) {
		case string:
			args[index] = terminalText(value)
		case error:
			args[index] = terminalText(value.Error())
		}
	}
	fmt.Printf(format, args...)
}

type checkedWriter struct{ io.Writer }

func (writer checkedWriter) Write(data []byte) (int, error) {
	n, err := writer.Writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

// Closing is part of output completion, and runs even after a failed write.
// stdout has no closer here because the caller does not own it.
func writeOutput(writer io.Writer, closer io.Closer, result output, asText bool) error {
	var writeErr error
	if asText {
		writeErr = writeText(writer, result)
	} else {
		encoder := json.NewEncoder(checkedWriter{writer})
		encoder.SetIndent("", "  ")
		writeErr = encoder.Encode(result)
	}
	if writeErr != nil {
		writeErr = fmt.Errorf("写入输出: %w", writeErr)
	}
	var closeErr error
	if closer != nil {
		if err := closer.Close(); err != nil {
			closeErr = fmt.Errorf("关闭输出文件: %w", err)
		}
	}
	return errors.Join(writeErr, closeErr)
}
