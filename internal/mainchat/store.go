// Package mainchat persists main-chat conversations as append-only jsonl
// files under <data-dir>/mainchats/<id>.jsonl.
//
// usher is single-process, so a per-store mutex is enough to serialize
// concurrent appends. There is no on-disk locking and no partitioning.
package mainchat

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Message struct {
	Role    string    `json:"role"`    // "user" | "agent"
	Content string    `json:"content"`
	Time    time.Time `json:"ts"`
}

type Chat struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

var idRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) (string, error) {
	if !idRE.MatchString(id) {
		return "", errors.New("invalid mainchat id")
	}
	return filepath.Join(s.dir, id+".jsonl"), nil
}

// Append writes msg to the chat's jsonl, creating the file if needed.
func (s *Store) Append(id string, msg Message) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if msg.Time.IsZero() {
		msg.Time = time.Now().UTC()
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// Read returns the chat's messages. limit > 0 keeps only the latest N.
// A missing file returns (nil, nil) — chats are created lazily on Append.
func (s *Store) Read(id string, limit int) ([]Message, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, nil
}

// List enumerates known chats by scanning the directory.
func (s *Store) List() ([]Chat, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Chat
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if !idRE.MatchString(id) {
			continue
		}
		title, _ := s.firstUserContent(id)
		out = append(out, Chat{ID: id, Title: title})
	}
	return out, nil
}

func (s *Store) firstUserContent(id string) (string, error) {
	msgs, err := s.Read(id, 0)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		if m.Role == "user" {
			return truncate(m.Content, 60), nil
		}
	}
	return "", nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
