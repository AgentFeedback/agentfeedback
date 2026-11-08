package sessions

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// readerCaptureOnly follows the environment through wrappers used by parsers.
func readerCaptureOnly(r io.Reader) bool {
	if r, ok := r.(interface{ CaptureOnly() bool }); ok {
		return r.CaptureOnly()
	}
	return false
}

// scanJSONL reads the complete lines of r into t's line bookkeeping: a
// final line without '\n' is neither passed on nor counted. line gets the
// offset of each complete line and its body without CR or LF, and reports
// whether it parsed as an entry; t.parsed counts those that did and
// t.badLines records the offsets of those that did not. t.head becomes the
// SHA-256 hex of the first complete line's body, t.size the bytes of the
// complete lines.
func scanJSONL(r io.Reader, t *transcript, line func(offset int64, body []byte) bool) error {
	ctx := context.Background()
	if r, ok := r.(interface{ Context() context.Context }); ok {
		ctx = r.Context()
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b, err := br.ReadBytes('\n')
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(b) > 0 && b[len(b)-1] == '\n' {
			body := bytes.TrimRight(b, "\r\n")
			if offset == 0 {
				sum := sha256.Sum256(body)
				t.head = hex.EncodeToString(sum[:])
			}
			if line(offset, body) {
				t.parsed++
			} else {
				t.badLines = append(t.badLines, offset)
			}
			offset += int64(len(b))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	t.size = offset

	return nil
}
