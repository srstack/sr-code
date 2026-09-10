package core

import "time"

// SessionMeta is the backend-neutral descriptor discovery needs to list a
// persisted agent session without loading its full transcript.
type SessionMeta struct {
	ID          string
	ParentID    string
	IsSubagent  bool
	AgentName   string
	Cwd         string
	Title       string
	Prompt      string
	StartedAt   time.Time
	LastEventAt time.Time
	LastInputAt time.Time
	Runtime     SessionRuntime
}

// TurnPart is one segment within a grouped assistant turn.
type TurnPart struct {
	Type       string `json:"type"`
	Content    string `json:"content"`
	ToolName   string `json:"toolName,omitempty"`
	ToolTarget string `json:"toolTarget,omitempty"`

	// Time is the transcript timestamp of the record that produced the part.
	Time time.Time `json:"ts,omitempty"`
	// DurationMs is a thinking part's span: to the next part, or to the turn
	// end when it is last. Stamped at turn completion, so live-streamed parts
	// carry none yet — the UI shows a bare "thinking" until it lands.
	DurationMs int64 `json:"duration_ms,omitempty"`

	// ToolUseID is parser bookkeeping used to join metadata follow-ups to the
	// tool part they enrich. It is never part of the public transcript shape.
	ToolUseID string `json:"-"`
}

// StampPartDurations fills thinking parts' DurationMs at turn completion:
// each spans to the next part's timestamp, the last to the turn's end.
func (t *Turn) StampPartDurations() {
	for i := range t.Parts {
		if t.Parts[i].Type != "thinking" || t.Parts[i].Time.IsZero() {
			continue
		}
		end := t.EndTime
		if i+1 < len(t.Parts) && !t.Parts[i+1].Time.IsZero() {
			end = t.Parts[i+1].Time
		}
		if end.IsZero() {
			continue
		}
		if d := end.Sub(t.Parts[i].Time); d > 0 {
			t.Parts[i].DurationMs = d.Milliseconds()
		}
	}
}

// TokenUsage is one turn's token accounting, mapped from the backend's
// per-message usage records. The JSON names are a frontend contract.
//
// Convention (both backends normalize to it): Input is UNCACHED input tokens;
// CacheRead/CacheWrite are counted separately, never inside Input. A turn's
// total input is Input + CacheRead + CacheWrite.
type TokenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read,omitempty"`
	CacheWrite int64 `json:"cache_write,omitempty"`
}

// Turn is a grouped, display-ready timeline entry shared by every backend.
type Turn struct {
	Role    string     `json:"role"`
	Content string     `json:"content,omitempty"`
	Parts   []TurnPart `json:"parts,omitempty"`
	Time    time.Time  `json:"ts"`
	Model   string     `json:"model,omitempty"`
	UUID    string     `json:"uuid,omitempty"`
	// Usage is the turn's token accounting: the SUM across the turn's
	// assistant messages (usher's display unit is the turn, not the
	// message). Nil when no message in the turn carried usage.
	Usage   *TokenUsage `json:"usage,omitempty"`
	EndTime time.Time   `json:"-"`
}

// Touch advances the server-side end timestamp when ts is usable.
func (t *Turn) Touch(ts time.Time) {
	if t != nil && !ts.IsZero() {
		t.EndTime = ts
	}
}
