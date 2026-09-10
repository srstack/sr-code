package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo) for v1 store reads
)

// This file is the v1 store reader. usher used to shell out to `opencode db`
// for every read, but the CLI silently TRUNCATES large result sets — exit 0,
// no error, newest rows missing — whenever a running TUI contends the store.
// Shadows were rewritten with those partial snapshots and the UI reverted to
// older states. A direct WAL reader sees a consistent snapshot, never blocks
// the writer, and returns complete rows, so all v1 reads go through here.
//
// The connection is strictly read-only (query_only pragma); schema knowledge
// stays limited to the three tables the sync needs (session/message/part).

// v1DB opens the store once per runtime. The path comes from
// `opencode db path` so nonstandard installs stay correct.
func (r *Runtime) v1DB(ctx context.Context) (*sql.DB, error) {
	r.storeOnce.Do(func() {
		out, err := exec.CommandContext(ctx, r.cmd, "db", "path").Output()
		if err != nil {
			r.storeErr = fmt.Errorf("opencode db path: %w", err)
			return
		}
		path := strings.TrimSpace(string(out))
		dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=query_only(true)"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			r.storeErr = err
			return
		}
		r.store = db
	})
	return r.store, r.storeErr
}

// queryTextRows runs a read-only query and returns every row with all
// columns as text (integers stringified, NULL as "") — the shape the old
// `opencode db --format tsv` path produced.
func queryTextRows(ctx context.Context, db *sql.DB, query string, args ...any) ([][]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			switch t := v.(type) {
			case nil:
				// stays ""
			case string:
				row[i] = t
			case []byte:
				row[i] = string(t)
			case int64:
				row[i] = strconv.FormatInt(t, 10)
			case float64:
				row[i] = strconv.FormatFloat(t, 'f', -1, 64)
			default:
				row[i] = fmt.Sprint(t)
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
