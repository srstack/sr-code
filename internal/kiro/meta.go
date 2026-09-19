package kiro

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/nexustar/usher/internal/core"
)

// ReadSessionMeta builds the discovery descriptor from session.json plus the
// transcript tail (last user prompt time, latest context usage).
func ReadSessionMeta(path string) (core.SessionMeta, error) {
	meta := core.SessionMeta{ID: filepath.Base(filepath.Dir(path))}
	s, err := readSessionJSON(filepath.Dir(path))
	if err != nil {
		return meta, err
	}
	meta.Title = s.Title
	if len(s.WorkspacePaths) > 0 {
		meta.Cwd = s.WorkspacePaths[0]
	} else if len(s.RootPaths) > 0 {
		meta.Cwd = s.RootPaths[0]
	}
	meta.StartedAt = s.CreatedAt
	meta.LastInputAt = s.LastModifiedAt
	meta.Runtime.Model = s.ModelID
	if st, err := os.Stat(path); err == nil {
		meta.LastEventAt = st.ModTime()
	}
	if meta.LastEventAt.IsZero() {
		meta.LastEventAt = s.LastModifiedAt
	}
	fillTail(path, &meta)
	return meta, nil
}

// fillTail scans the end of messages.jsonl for the last genuine user prompt
// (LastInputAt) and the latest contextUsage percentage.
func fillTail(path string, meta *core.SessionMeta) {
	const tailBytes = 256 << 10
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	start := int64(0)
	if st.Size() > tailBytes {
		start = st.Size() - tailBytes
	}
	if _, err := f.Seek(start, 0); err != nil {
		return
	}
	buf := make([]byte, st.Size()-start)
	n, _ := f.Read(buf)
	buf = buf[:n]
	lines := splitLines(buf)
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // drop the partial first line
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var e entry
		if json.Unmarshal(lines[i], &e) != nil {
			continue
		}
		switch e.Payload.Type {
		case "user":
			if meta.LastInputAt.IsZero() || e.Timestamp.After(meta.LastInputAt) {
				meta.LastInputAt = e.Timestamp
			}
			return // newest user record found; older lines don't matter
		}
	}
}

func splitLines(buf []byte) [][]byte {
	var out [][]byte
	for len(buf) > 0 {
		i := 0
		for i < len(buf) && buf[i] != '\n' {
			i++
		}
		line := buf[:i]
		if len(line) > 0 {
			out = append(out, line)
		}
		if i == len(buf) {
			break
		}
		buf = buf[i+1:]
	}
	return out
}
