package httpx

import (
	"bufio"
	"io"
	"strings"
)

// SSEReader parses a Server-Sent Events stream.
type SSEReader struct {
	r   *bufio.Reader
	err error
}

// NewSSEReader reads events from r.
func NewSSEReader(r io.Reader) *SSEReader { return &SSEReader{r: bufio.NewReaderSize(r, 64<<10)} }

// Next returns the next event: its name ("" when unset) and its data (multi-line data joined
// with "\n"). Comments (":") and blocks without data are skipped; "[DONE]" is returned as is;
// io.EOF follows the last event.
func (s *SSEReader) Next() (event, data string, err error) {
	var ev string
	var lines []string
	pending := false
	for {
		line, rerr := s.r.ReadString('\n')
		if rerr != nil && line == "" {
			if pending {
				s.err = rerr
				return ev, strings.Join(lines, "\n"), nil
			}
			if s.err != nil {
				return "", "", s.err
			}
			return "", "", rerr
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if pending {
				return ev, strings.Join(lines, "\n"), nil
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev, pending = value, true
			case "data":
				lines, pending = append(lines, value), true
			}
		}
		if rerr != nil { // last line without a trailing newline
			if pending {
				s.err = rerr
				return ev, strings.Join(lines, "\n"), nil
			}
			return "", "", rerr
		}
	}
}
