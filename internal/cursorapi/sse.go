package cursorapi

import (
	"bufio"
	"io"
	"strings"
)

// ParseSSE reads a text/event-stream body and calls fn for every complete
// event. It handles CRLF line endings, comment lines, and multi-line data.
// It returns when the body ends, on read error, or when fn returns an error.
func ParseSSE(r io.Reader, fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var (
		ev    Event
		data  []string
		dirty bool
	)
	flush := func() error {
		if !dirty {
			return nil
		}
		if len(data) > 0 {
			ev.Data = strings.Join(data, "\n")
		}
		out := ev
		ev, data, dirty = Event{}, nil, false
		return fn(out)
	}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Type, dirty = value, true
		case "id":
			ev.ID, dirty = value, true
		case "data":
			data, dirty = append(data, value), true
		case "retry":
			// Reconnect hints are handled by the caller's backoff.
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}
