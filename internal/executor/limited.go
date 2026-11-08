package executor

import "bytes"

// maxOutputBytes is the most output a single command may return. A message
// to Gotify is read on a phone, and an unbounded "journalctl" or "find" would
// otherwise grow the plugin's memory with the size of the log.
const maxOutputBytes = 64 * 1024

// truncatedMarker is appended to output that was cut at maxOutputBytes, so the
// reader knows the end of the text is missing.
const truncatedMarker = "\n… (truncated)"

// limitedBuffer is an io.Writer that keeps the first limit bytes written to it
// and silently drops the rest.
type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

// Write stores as much of data as fits under the limit. It always reports the
// full length as written: a short write would make os/exec stop copying and
// leave the child blocked on a full pipe until the timeout.
func (l *limitedBuffer) Write(data []byte) (int, error) {
	room := l.limit - l.buffer.Len()

	if len(data) > room {
		l.truncated = true
		l.buffer.Write(data[:max(room, 0)])
		return len(data), nil
	}

	l.buffer.Write(data)
	return len(data), nil
}

// String returns the kept output, followed by truncatedMarker when something
// was dropped.
func (l *limitedBuffer) String() string {
	if l.truncated {
		return l.buffer.String() + truncatedMarker
	}
	return l.buffer.String()
}
