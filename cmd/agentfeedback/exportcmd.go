package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
)

// runExport streams the export body byte for byte to stdout and checks, as
// it goes, that the last line is a trailer whose count and sha256 match the
// record lines.
func runExport(args []string, _ io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("export")
	afterID := fs.Int64("after-id", 0, "only rows with a larger id")
	kind := fs.String("kind", "", "only this kind")
	since := fs.String("since", "", "RFC 3339 time or <n>m, <n>h, <n>d, <n>w ago, inclusive, on created_at")
	limit := fs.Int("limit", 0, "at most this many rows, 1-500")
	if err := parseFlags(fs, args, stderr); err != nil {
		return errFlags("export", err)
	}
	if fs.NArg() != 0 {
		return errArgs("export", "export [--after-id N] [--kind K] [--since T] [--limit N]")
	}
	set := visited(fs)
	q := url.Values{}
	if set["after-id"] {
		q.Set("after_id", strconv.FormatInt(*afterID, 10))
	}
	if set["kind"] {
		q.Set("kind", *kind)
	}
	if set["since"] {
		t, err := parseTimeFlag("since", *since)
		if err != nil {
			return err
		}
		q.Set("since", t)
	}
	if set["limit"] {
		q.Set("limit", strconv.Itoa(*limit))
	}

	c, err := apiClient(os.Getenv, stderr)
	if err != nil {
		return err
	}
	body, _, err := c.Stream(context.Background(), "/api/v1/export", q)
	if err != nil {
		return apiErr(err, stderr)
	}
	defer func() { _ = body.Close() }()

	return copyExport(stdout, body)
}

// exportLineMax caps one export line; a var so tests can lower it.
var exportLineMax = 64 << 20

// copyExport copies an export to w and verifies its trailer.
func copyExport(w io.Writer, r io.Reader) error {
	each := func(line []byte) error {
		_, err := w.Write(line)

		return err
	}

	return walkExport(r, each, nil)
}

// walkExport reads an export line by line and verifies its trailer: each,
// when not nil, gets every line as it arrives, and record, when not nil,
// every record line once the line after it shows it is not the trailer.
// Lines keep their newline and are only valid during the call. At most one
// line (capped at exportLineMax) is held beside the one being read. An
// error of each or record is returned as it is.
func walkExport(r io.Reader, each, record func(line []byte) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	sum := sha256.New()
	var lines, records int64
	var pending, line []byte // pending is a record unless it is the last line
	for {
		var err error
		line, err = readLine(br, line[:0])
		if len(line) > 0 {
			if each != nil {
				if werr := each(line); werr != nil {
					return werr
				}
			}
			if lines >= 2 {
				sum.Write(pending)
				records++
				if record != nil {
					if rerr := record(pending); rerr != nil {
						return rerr
					}
				}
			}
			pending, line = line, pending
			lines++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errLineTooLong) {
			return errExportIncomplete(fmt.Sprintf("a line is longer than %d MiB", exportLineMax>>20))
		}
		if err != nil {
			return errExportIncomplete(fmt.Sprintf("the stream broke: %v", err))
		}
	}
	if lines < 2 {
		return errExportIncomplete("there is no trailer line")
	}
	var trailer struct {
		Complete bool   `json:"export_complete"`
		Count    *int64 `json:"count"`
		SHA256   string `json:"sha256"`
	}
	if json.Unmarshal(pending, &trailer) != nil || !trailer.Complete || trailer.Count == nil {
		return errExportIncomplete("the last line is not the trailer")
	}
	if *trailer.Count != records {
		return errExportIncomplete(fmt.Sprintf("the trailer counts %d records, the body holds %d", *trailer.Count, records))
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != trailer.SHA256 {
		return errExportIncomplete("the trailer sha256 does not match the record lines")
	}

	return nil
}

var errLineTooLong = errors.New("line too long")

// readLine appends one line, its newline included, to buf; a line longer
// than exportLineMax is errLineTooLong with nothing more read into buf.
func readLine(br *bufio.Reader, buf []byte) ([]byte, error) {
	for {
		frag, err := br.ReadSlice('\n')
		if len(buf)+len(frag) > exportLineMax {
			return buf[:0], errLineTooLong
		}
		buf = append(buf, frag...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return buf, err
		}
	}
}
