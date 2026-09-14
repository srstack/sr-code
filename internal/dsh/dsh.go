// Package dsh reads dsh's session logs and projects their append-origin human
// transcript into usher's core display model.
//
// A session log lives at <dsh-home>/sessions/<project-slug>/<session-id>/
// session.vN.jsonl[.zstd]: frame 0 is a session header, then one JSON event per
// frame. dsh wraps the log in a "surface" layer — an ordered view of the four
// message-producing event types (system/message, user/message,
// assistant/message, tool/result). Replacements shadow earlier surface ranges
// in the model-visible transcript; this reader follows the durable human view
// instead, taking only append-origin events (plus the compaction checkpoint,
// which is a replacement but is the only trace the human transcript keeps of a
// compaction). tool/call is a log-only record used to time its tool part.
//
// The package is read-only by design: dsh is registered with a nil Runtime, so
// usher never spawns or drives a dsh process. ReadTurns renders what is on
// disk; the Assembler stub exists only to satisfy backend.Transcript.
package dsh

import (
	"bufio"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/codexrollout"
	"github.com/nexustar/usher/internal/core"
)

// Transcript implements backend.Transcript for dsh session logs.
type Transcript struct{}

// wireEvent is the uniform dsh event envelope. surfaceOp marks a
// message-producing event as an append ("append") or a replacement
// ({"op":"replace",startSeq,endSeq}); older logs may omit it.
type wireEvent struct {
	Type      string          `json:"type"`
	Seq       int64           `json:"seq"`
	Time      int64           `json:"time"` // epoch milliseconds
	Data      json.RawMessage `json:"data"`
	SurfaceOp json.RawMessage `json:"surfaceOp"`
}

// contentBlock is one block of a user message body.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// userData is a user/message payload: a Message carrying content plus the
// source that tells a real prompt from injected context.
type userData struct {
	Content []contentBlock `json:"content"`
	Source  struct {
		Kind   string `json:"kind"`
		Plugin string `json:"plugin"`
	} `json:"source"`
}

// assistantBlock is one content block of an assistant message.
type assistantBlock struct {
	Type      string `json:"type"`      // text | reasoning | tool-call
	Text      string `json:"text"`      // text, reasoning
	ID        string `json:"id"`        // tool-call
	Name      string `json:"name"`      // tool-call
	Arguments string `json:"arguments"` // tool-call: raw JSON string
}

// assistantData is an assistant/message payload.
type assistantData struct {
	Turn    int `json:"turn"`
	Step    int `json:"step"`
	Message struct {
		Content []assistantBlock `json:"content"`
		Source  struct {
			Kind     string `json:"kind"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		} `json:"source"`
	} `json:"message"`
	Usage       *usage        `json:"usage"`
	Stream      []streamEntry `json:"stream"`
	Interrupted bool          `json:"interrupted"`
}

// streamEntry is one record of an assistant message's stream log. Only the
// per-block chunk records carry time0/index — the epoch-ms a block started
// streaming and its index in the message's content array; the surrounding
// chunk/usage/finish records are ignored.
type streamEntry struct {
	Type  string `json:"type"`
	Time0 int64  `json:"time0"`
	Index int    `json:"index"`
}

// summaryData is a compaction/summary payload: the condensation text dsh keeps
// for a compaction checkpoint.
type summaryData struct {
	Content []contentBlock `json:"content"`
	Text    string         `json:"text"`
}

// maxPartBytes caps a single TurnPart.Content, mirroring internal/jsonl's
// clampBody: one huge file or model dump must not bloat the transcript payload.
const maxPartBytes = 32 * 1024

// usage is dsh's per-message token accounting. inputTokens is already uncached
// (usher's core.TokenUsage convention), so it maps straight across.
type usage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

// toolCallData is a tool/call payload: the log-only half of a tool part, kept
// for the call's own timestamp and name/target.
type toolCallData struct {
	Turn      int    `json:"turn"`
	CallID    string `json:"callId"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// resultBlock is one block of a tool/result message. A tool-result block nests
// its display text in content[].
type resultBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	ToolCallID string          `json:"toolCallId"`
	IsError    bool            `json:"isError"`
	Content    json.RawMessage `json:"content"`
}

// toolResultData is a tool/result payload.
type toolResultData struct {
	Turn    int `json:"turn"`
	Message struct {
		Content []resultBlock `json:"content"`
		Source  struct {
			CallID string `json:"callId"`
		} `json:"source"`
	} `json:"message"`
	Error json.RawMessage `json:"error"`
}

// ReadTurns returns the human transcript of the dsh session at path, grouped so
// each dsh turn number is one assistant Turn whose Parts interleave reasoning,
// text, and tool call/result pairs. limit > 0 keeps only the most recent N
// turns; total is the count before that trim (same contract as internal/jsonl).
func (Transcript) ReadTurns(path string, limit int) (turns []core.Turn, total int, err error) {
	rc, err := codexrollout.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer rc.Close()

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	b := newBuilder()
	first := true
	for sc.Scan() {
		var ev wireEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			first = false
			continue // malformed line: skip, never fail the whole read
		}
		if first {
			first = false
			if ev.Type == "session" {
				continue // frame 0 header
			}
		}
		b.event(ev)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	return b.finish(limit)
}

// NewAssembler returns a stub: this reader is read-only, and no dsh Runtime is
// ever registered, so runSend never drives a live assembler. It exists only to
// satisfy backend.Transcript.
func (Transcript) NewAssembler() backend.Assembler { return Assembler{} }

// IsTurnComplete is always false: dsh logs are read in batch, not streamed.
func (Transcript) IsTurnComplete([]byte) bool { return false }

// IsTurnAborted is always false: dsh logs are read in batch, not streamed.
func (Transcript) IsTurnAborted([]byte) bool { return false }

// Assembler is the inert assembler stub returned by NewAssembler.
type Assembler struct{}

// FeedLine consumes nothing and produces no turn.
func (Assembler) FeedLine([]byte) ([]core.Turn, *core.TurnPart) { return nil, nil }

// Flush returns nil: the stub never accumulates a turn.
func (Assembler) Flush() *core.Turn { return nil }

// Model returns the stub's model, always empty.
func (Assembler) Model() string { return "" }

// partRef locates a tool part inside a turn by index, so a later tool/result
// can fill it in place even after more parts are appended.
type partRef struct {
	turn *core.Turn
	idx  int
}

type toolMeta struct {
	name   string
	target string
}

// builder folds a session's events into turns. It is single-use and not
// goroutine-safe.
type builder struct {
	entries   []entry
	asst      map[int]*core.Turn
	callParts map[string]partRef
	callTimes map[string]time.Time
	callMeta  map[string]toolMeta
	// partSeq records the event seq that produced each turn's parts, so finish
	// can order them by seq rather than by arrival.
	partSeq map[*core.Turn][]int64
	// compactSummary is the most recent compaction/summary text, consumed by the
	// checkpoint marker that follows it.
	compactSummary string
}

type entry struct {
	seq  int64
	turn *core.Turn
}

func newBuilder() *builder {
	return &builder{
		asst:      map[int]*core.Turn{},
		callParts: map[string]partRef{},
		callTimes: map[string]time.Time{},
		callMeta:  map[string]toolMeta{},
		partSeq:   map[*core.Turn][]int64{},
	}
}

// event dispatches one event envelope. Unknown event types are skipped: usher
// must render historical and evolving logs, so an unrecognized record (or a
// non-append surface op) is never fatal — a deliberate deviation from dsh's
// strict ignorable rule.
func (b *builder) event(ev wireEvent) {
	switch ev.Type {
	case "user/message":
		var d userData
		if json.Unmarshal(ev.Data, &d) != nil {
			return
		}
		if isCompact(d) {
			// The compaction checkpoint is a replacement, but it is the only
			// durable trace of the compaction in the human transcript.
			b.marker(ev)
			return
		}
		if !isAppendOp(ev.SurfaceOp) {
			return // replacement copies are model-only
		}
		b.user(ev, d)
	case "compaction/summary":
		b.summary(ev)
	case "assistant/message":
		if !isAppendOp(ev.SurfaceOp) {
			return
		}
		b.assistant(ev)
	case "tool/result":
		if !isAppendOp(ev.SurfaceOp) {
			return
		}
		b.toolResult(ev)
	case "tool/call":
		b.toolCall(ev)
	default:
		// system/message (the per-request system prompt), turn/step boundaries,
		// agent/inbox/spliced, approvals, goals — all log-only for a human view.
		return
	}
}

// user projects a user/message. A real prompt (source.kind=="user") is a user
// turn; any other source is context injection, rendered as a collapsed system
// turn prefixed with its kind. A message with no source is treated as a user
// prompt (older/seeded logs carry none).
func (b *builder) user(ev wireEvent, d userData) {
	text := joinText(d.Content)
	switch d.Source.Kind {
	case "", "user":
		if text == "" {
			return
		}
		b.add(ev.Seq, &core.Turn{Role: "user", Content: text, Time: msTime(ev.Time)})
	default:
		b.add(ev.Seq, &core.Turn{Role: "system", Content: "[" + d.Source.Kind + "] " + text, Time: msTime(ev.Time)})
	}
}

func (b *builder) assistant(ev wireEvent) {
	var d assistantData
	if json.Unmarshal(ev.Data, &d) != nil {
		return
	}
	ts := msTime(ev.Time)
	t := b.asst[d.Turn]
	if t == nil {
		t = &core.Turn{Role: "assistant", Time: ts}
		b.asst[d.Turn] = t
		b.add(ev.Seq, t)
	}
	t.Touch(ts)

	// Model is the LAST assistant message's model (provider as fallback).
	if d.Message.Source.Model != "" {
		t.Model = d.Message.Source.Model
	} else if d.Message.Source.Provider != "" {
		t.Model = d.Message.Source.Provider
	}
	if d.Usage != nil {
		if t.Usage == nil {
			t.Usage = &core.TokenUsage{}
		}
		t.Usage.Input += d.Usage.InputTokens
		t.Usage.Output += d.Usage.OutputTokens
		t.Usage.CacheRead += d.Usage.CacheReadTokens
		t.Usage.CacheWrite += d.Usage.CacheWriteTokens
	}

	// Each block takes its start time from the message's stream record when dsh
	// logged one, so a reasoning block emitted in the same message as a tool
	// call still spans a positive "thought for Ns" rather than sharing the
	// message's end time. Absent a stream record (older logs), the whole
	// message's timestamp is the fallback.
	blockTimes := streamBlockTimes(d.Stream)
	produced := 0
	for i, blk := range d.Message.Content {
		bt := ts
		if v, ok := blockTimes[i]; ok {
			bt = msTime(v)
		}
		switch blk.Type {
		case "reasoning":
			if blk.Text == "" {
				continue
			}
			b.addPart(t, ev.Seq, core.TurnPart{Type: "thinking", Content: blk.Text, Time: bt})
			produced++
		case "text":
			if blk.Text == "" {
				continue
			}
			b.addPart(t, ev.Seq, core.TurnPart{Type: "text", Content: blk.Text, Time: bt})
			produced++
		case "tool-call":
			target := toolTarget(blk.Name, blk.Arguments)
			idx := b.addPart(t, ev.Seq, core.TurnPart{
				Type:       "tool",
				ToolName:   blk.Name,
				ToolTarget: target,
				ToolUseID:  blk.ID,
				Time:       bt,
			})
			if blk.ID != "" {
				b.callParts[blk.ID] = partRef{turn: t, idx: idx}
				if _, ok := b.callMeta[blk.ID]; !ok {
					b.callMeta[blk.ID] = toolMeta{name: blk.Name, target: target}
				}
			}
			produced++
		}
	}
	// An interrupted message that emitted nothing still happened: keep dsh's
	// "stopped" hint without inventing content it never produced.
	if d.Interrupted && produced == 0 {
		b.addPart(t, ev.Seq, core.TurnPart{Type: "text", Content: "— stopped —", Time: ts})
	}
}

// streamBlockTimes maps a message content-block index to the epoch-ms at which
// dsh began streaming it. Only the reasoning/text/tool-call chunk records carry
// a start time; every other stream record is ignored.
func streamBlockTimes(entries []streamEntry) map[int]int64 {
	var out map[int]int64
	for _, e := range entries {
		if e.Time0 == 0 {
			continue
		}
		switch e.Type {
		case "reasoning-chunks", "text-chunks", "tool-call-chunks":
			if out == nil {
				out = map[int]int64{}
			}
			out[e.Index] = e.Time0
		}
	}
	return out
}

// summary records a compaction/summary's condensation text for the checkpoint
// marker that follows it.
func (b *builder) summary(ev wireEvent) {
	var d summaryData
	if json.Unmarshal(ev.Data, &d) != nil {
		return
	}
	if text := joinText(d.Content); text != "" {
		b.compactSummary = text
	} else if strings.TrimSpace(d.Text) != "" {
		b.compactSummary = d.Text
	}
}

// addPart appends a part to t and records the seq that produced it, returning
// the part's index.
func (b *builder) addPart(t *core.Turn, seq int64, p core.TurnPart) int {
	idx := len(t.Parts)
	t.Parts = append(t.Parts, p)
	b.partSeq[t] = append(b.partSeq[t], seq)
	return idx
}

// toolCall records the log-only half of a call: its timestamp and name/target,
// used to time and label the tool part an assistant message already emitted.
func (b *builder) toolCall(ev wireEvent) {
	var d toolCallData
	if json.Unmarshal(ev.Data, &d) != nil || d.CallID == "" {
		return
	}
	b.callTimes[d.CallID] = msTime(ev.Time)
	if meta, ok := b.callMeta[d.CallID]; ok && meta.name != "" {
		return // the assistant block is the richer source; keep it
	}
	b.callMeta[d.CallID] = toolMeta{name: d.Name, target: toolTarget(d.Name, d.Arguments)}
}

// toolResult pairs a result with the tool part its callId identifies (searching
// every turn) and fills in content and duration. An unmatched result is
// preserved as an orphan tool part rather than dropped.
func (b *builder) toolResult(ev wireEvent) {
	var d toolResultData
	if json.Unmarshal(ev.Data, &d) != nil {
		return
	}
	callID, text, isErr, errCode := d.result()
	content := text
	if isErr {
		content = errorContent(errCode, text)
	}
	ts := msTime(ev.Time)

	if ref, ok := b.callParts[callID]; ok && callID != "" {
		delete(b.callParts, callID)
		p := &ref.turn.Parts[ref.idx]
		p.Content = content
		start := p.Time
		if ct, ok := b.callTimes[callID]; ok {
			start = ct
		}
		if !ts.IsZero() && !start.IsZero() {
			if dur := ts.Sub(start); dur > 0 {
				p.DurationMs = dur.Milliseconds()
			}
		}
		ref.turn.Touch(ts)
		return
	}

	t := b.asst[d.Turn]
	if t == nil {
		t = &core.Turn{Role: "assistant", Time: ts}
		b.asst[d.Turn] = t
		b.add(ev.Seq, t)
	}
	t.Touch(ts)
	meta := b.callMeta[callID]
	part := core.TurnPart{Type: "tool", ToolName: meta.name, ToolTarget: meta.target, ToolUseID: callID, Content: content, Time: ts}
	if ct, ok := b.callTimes[callID]; ok && !ct.IsZero() && !ts.IsZero() {
		part.Time = ct
		if dur := ts.Sub(ct); dur > 0 {
			part.DurationMs = dur.Milliseconds()
		}
	}
	b.addPart(t, ev.Seq, part)
}

// marker appends the compaction checkpoint turn in seq position, carrying a
// short excerpt of the paired compaction/summary so the condensation is not
// lost. Without a summary the content stays the exact marker string.
func (b *builder) marker(ev wireEvent) {
	content := "Context compacted"
	if s := strings.TrimSpace(b.compactSummary); s != "" {
		content += "\n\n" + excerpt(s, 200)
	}
	b.compactSummary = ""
	b.add(ev.Seq, &core.Turn{Role: "system", Content: content, Time: msTime(ev.Time)})
}

func (b *builder) add(seq int64, t *core.Turn) {
	b.entries = append(b.entries, entry{seq: seq, turn: t})
}

// finish orders turns by their first event seq, drops empty rows, orders each
// turn's parts by the seq that produced them, caps part payloads, stamps
// reasoning durations, and applies the tail limit.
func (b *builder) finish(limit int) ([]core.Turn, int, error) {
	sort.SliceStable(b.entries, func(i, j int) bool { return b.entries[i].seq < b.entries[j].seq })
	turns := make([]core.Turn, 0, len(b.entries))
	for _, e := range b.entries {
		t := e.turn
		if t.Role == "assistant" {
			if len(t.Parts) == 0 {
				continue
			}
			b.orderParts(t)
			for i := range t.Parts {
				t.Parts[i].Content = clampPart(t.Parts[i].Content)
			}
			t.StampPartDurations()
		} else if strings.TrimSpace(t.Content) == "" {
			continue
		}
		turns = append(turns, *t)
	}
	total := len(turns)
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, total, nil
}

// result extracts a tool result's callId, joined display text, error flag, and
// (when dsh recorded a top-level error) its code, falling back to the
// tool-result block's own callId when the message source carries none.
func (d toolResultData) result() (callID, text string, isErr bool, errCode string) {
	callID = d.Message.Source.CallID
	var texts []string
	for _, blk := range d.Message.Content {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				texts = append(texts, blk.Text)
			}
		case "tool-result":
			if callID == "" && blk.ToolCallID != "" {
				callID = blk.ToolCallID
			}
			if blk.IsError {
				isErr = true
			}
			texts = append(texts, flattenResultContent(blk.Content)...)
		}
	}
	if len(d.Error) > 0 && string(d.Error) != "null" {
		isErr = true
		var e struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(d.Error, &e) == nil {
			errCode = e.Code
		}
	}
	return callID, strings.Join(texts, "\n"), isErr, errCode
}

// errorContent renders a failed tool result for display, prefixing dsh's error
// code when it recorded one: "error: <code>: <text>", dropping empty pieces.
func errorContent(code, text string) string {
	parts := []string{"error"}
	if code != "" {
		parts = append(parts, code)
	}
	if text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, ": ")
}

// orderParts stably reorders a turn's parts by the event seq that produced them,
// preserving document order for parts that share a seq (the blocks of one
// assistant message). It is a no-op when the bookkeeping is absent or aligned.
func (b *builder) orderParts(t *core.Turn) {
	seqs := b.partSeq[t]
	if len(seqs) != len(t.Parts) || len(seqs) < 2 {
		return
	}
	order := make([]int, len(seqs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return seqs[order[i]] < seqs[order[j]] })
	parts := make([]core.TurnPart, len(t.Parts))
	for dst, src := range order {
		parts[dst] = t.Parts[src]
	}
	t.Parts = parts
}

// clampPart bounds a part's content so one huge file or model dump cannot bloat
// the transcript payload, mirroring internal/jsonl's clampBody.
func clampPart(s string) string {
	if len(s) <= maxPartBytes {
		return s
	}
	return s[:maxPartBytes] + "\n… (truncated)"
}

// excerpt returns the first n runes of s, with an ellipsis when clamped.
func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// flattenResultContent pulls text out of a tool-result block's nested content,
// which is either a bare string or an array of text blocks.
func flattenResultContent(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// isCompact reports the compaction checkpoint marker: a plugin user/message
// whose plugin is "compact".
func isCompact(d userData) bool {
	return d.Source.Kind == "plugin" && d.Source.Plugin == "compact"
}

// isAppendOp reports whether a surface event belongs to the durable human
// transcript. An absent marker (older logs) or the literal "append" appends;
// a replacement object (or any other marker) does not.
func isAppendOp(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "append"
	}
	return false
}

func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// joinText joins a message's text blocks, the display body for user/system
// turns.
func joinText(blocks []contentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toolTarget picks the most informative argument to show beside a tool name.
// Keys are tried per tool; unknown tools fall back to the first string value in
// document order. The result is capped at 120 runes.
func toolTarget(name, argsRaw string) string {
	if strings.TrimSpace(argsRaw) == "" {
		return ""
	}
	return truncateRunes(pickArg(argsRaw, toolTargetKeys(name)), 120)
}

// toolTargetKeys lists the candidate argument keys for a tool, in preference
// order. A nil slice means "any string, in document order".
func toolTargetKeys(name string) []string {
	switch strings.ToLower(name) {
	case "bash", "shell":
		return []string{"command"}
	case "read":
		return []string{"file_path", "path", "url"}
	case "write", "edit":
		return []string{"file_path", "path"}
	case "grep", "glob":
		return []string{"pattern"}
	case "webfetch", "web_fetch", "websearch", "web_search":
		return []string{"url", "query"}
	default:
		return nil
	}
}

// pickArg returns the first non-empty string argument among keys, or — when
// keys is empty — the first non-empty string value in the object's document
// order (Go maps do not preserve order, so this decodes the token stream).
func pickArg(argsRaw string, keys []string) string {
	if len(keys) > 0 {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(argsRaw), &m) != nil {
			return ""
		}
		for _, k := range keys {
			var s string
			if raw, ok := m[k]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
				return s
			}
		}
		return ""
	}
	return firstStringValue(argsRaw)
}

// firstStringValue walks a JSON object's tokens in document order and returns
// the first string-valued member. "" when the payload is not an object or
// carries no string value.
func firstStringValue(argsRaw string) string {
	dec := json.NewDecoder(strings.NewReader(argsRaw))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return ""
	}
	for dec.More() {
		if _, err := dec.Token(); err != nil { // key
			return ""
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return ""
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// truncateRunes caps s at n runes (no ellipsis: the caller's value is a display
// target, not prose).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
