package capture

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

var (
	errTranscriptLarge = errors.New("scrubbed transcript exceeds envelope limit")
	errTranscriptUTF8  = errors.New("transcript is not UTF-8")
)

// These boundaries mirror scrub.String's only cross-line rules. Private-key
// bodies are discarded incrementally; incomplete bearer/assignment prefixes
// stay on their original line until their value arrives.
var (
	privateBegin      = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	privateEnd        = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	scrubContinuation = regexp.MustCompile(`(?i)(?:\bbearer\s+|["']?[A-Za-z0-9_.-]*(?:password|passwd|client_secret|secret|auth_token|token|api[_-]?key|access_key|private_key)["']?\s*(?:[:=]\s*)?)$`)
)

// scrubTranscript retains at most the scrubbed envelope plus the current
// record and unfinished cross-line prefix. Source records use the same
// record-sized buffering as session parsing; total raw source size is uncapped.
// JSON strings are decoded before text rules, so escaped credentials cannot
// bypass scrubbing. Nothing is written to disk.
func scrubTranscript(r io.Reader, limit int) (string, scrub.Counts, error) {
	counts := scrub.Counts{}
	var output strings.Builder
	pending := ""
	inKey := false
	flush := func(final bool) error {
		var c scrub.Counts
		pending, c = scrub.String(pending)
		counts.Add(c)
		end := len(pending)
		if !final {
			if match := scrubContinuation.FindStringIndex(pending); match != nil {
				end = strings.LastIndexByte(pending[:match[0]], '\n') + 1
			} else if !strings.HasSuffix(pending, "\n") {
				end = strings.LastIndexByte(pending, '\n') + 1
			}
		}
		if len(pending) > limit-output.Len() {
			return errTranscriptLarge
		}
		output.WriteString(pending[:end])
		pending = pending[end:]
		return nil
	}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			if !utf8.Valid(line) {
				return "", counts, errTranscriptUTF8
			}
			body := bytes.TrimSuffix(line, []byte{'\n'})
			body = bytes.TrimSuffix(body, []byte{'\r'})
			clean, c, err := scrub.JSON(body)
			var text string
			if err == nil && c.Total() > 0 {
				text = string(clean) + string(line[len(body):])
				counts.Add(c)
			} else {
				text = string(line)
			}
			for len(text) > 0 {
				if inKey {
					end := privateEnd.FindStringIndex(text)
					if end == nil {
						break
					}
					text, inKey = text[end[1]:], false
					continue
				}
				start := privateBegin.FindStringIndex(text)
				if start == nil {
					pending += text
					break
				}
				pending += text[:start[0]] + scrub.Marker("private_key")
				counts["private_key"]++
				text, inKey = text[start[1]:], true
			}
			if !inKey {
				if err := flush(false); err != nil {
					return "", counts, err
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", counts, readErr
			}
			if err := flush(true); err != nil {
				return "", counts, err
			}
			return output.String(), counts, nil
		}
	}
}
