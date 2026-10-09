package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/slack-go/slack"
)

// entry is one line of the transcript: a Slack message in or out, a button
// tap, a reaction, a canvas, or what chat routing did with a message. It never
// holds a token or an HTTP header.
type entry struct {
	LoggedAt   string   `json:"logged_at"`
	Dir        string   `json:"dir"`  // in | out
	Kind       string   `json:"kind"` // dm thread_reply button reaction canvas contract followup milestone chat_routed error
	Channel    string   `json:"channel,omitempty"`
	TS         string   `json:"ts,omitempty"`
	ThreadTS   string   `json:"thread_ts,omitempty"`
	User       string   `json:"user,omitempty"` // inbound only
	Text       string   `json:"text"`           // full, never cut
	ContractID string   `json:"contract_id,omitempty"`
	RunID      string   `json:"run_id,omitempty"`
	Workflow   string   `json:"workflow,omitempty"`
	CanvasURL  string   `json:"canvas_url,omitempty"`
	Title      string   `json:"title,omitempty"`     // canvas
	Value      string   `json:"value,omitempty"`     // button: the tapped action's value
	Answer     string   `json:"answer,omitempty"`    // button: what kami recorded
	Routed     string   `json:"routed,omitempty"`    // chat_routed: signalled | queued | started
	Delivered  []string `json:"delivered,omitempty"` // chat_routed signalled: the runs that took it
	Error      string   `json:"error,omitempty"`
}

func (a *adapter) transcriptPath() string { return filepath.Join(a.dir, "slack-transcript.ndjson") }

// record appends e to the transcript. One O_APPEND write per line, so serve
// and script-step dm/canvas processes never interleave. A failure is only
// logged: the transcript must never stop a message.
func (a *adapter) record(e entry) {
	e.LoggedAt = time.Now().Format(time.RFC3339Nano)
	if e.RunID == "" && e.Dir == "out" {
		e.RunID = a.run
	}
	raw, _ := json.Marshal(e)
	err := os.MkdirAll(a.dir, 0o755)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(a.transcriptPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
			_, err = f.Write(append(raw, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		log.Printf("kami-slack: transcript: %v", err)
	}
}

// readTranscript reads every entry; a missing file is an empty transcript.
func readTranscript(path string) ([]entry, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var e entry
		if json.Unmarshal(line, &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// --- log -----------------------------------------------------------------------

type filter struct{ run, contract, thread string }

func (f filter) match(e entry) bool {
	return (f.run == "" || e.RunID == f.run || contains(e.Delivered, f.run)) &&
		(f.contract == "" || e.ContractID == f.contract) &&
		(f.thread == "" || e.ThreadTS == f.thread || e.TS == f.thread)
}

// printLog prints the matching entries; with follow it then waits for new
// lines until ctx ends.
// ponytail: follow polls every 500ms; a truncated or rotated file is not reopened.
func printLog(ctx context.Context, w io.Writer, path string, f filter, follow, asJSON bool) error {
	var fh *os.File
	for {
		var err error
		if fh, err = os.Open(path); err == nil {
			break
		}
		if !follow || !errors.Is(err, os.ErrNotExist) {
			if errors.Is(err, os.ErrNotExist) {
				return nil // nothing logged yet
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
	defer fh.Close()
	rd := bufio.NewReader(fh)
	var part []byte
	for {
		line, err := rd.ReadBytes('\n')
		part = append(part, line...)
		if err == nil {
			var e entry
			if json.Unmarshal(part, &e) == nil && f.match(e) {
				if asJSON {
					w.Write(part)
				} else {
					fmt.Fprintln(w, e.String())
				}
			}
			part = nil
			continue
		}
		if err != io.EOF {
			return err
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e entry) String() string {
	t, _ := time.Parse(time.RFC3339Nano, e.LoggedAt)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-3s %-12s", t.Local().Format("2006-01-02 15:04:05"), e.Dir, e.Kind)
	for _, kv := range [][2]string{{"ts", e.TS}, {"thread", e.ThreadTS}, {"user", e.User}, {"contract", e.ContractID},
		{"run", e.RunID}, {"workflow", e.Workflow}, {"routed", e.Routed}, {"delivered", strings.Join(e.Delivered, ",")},
		{"value", e.Value}, {"answer", e.Answer}, {"title", e.Title}, {"url", e.CanvasURL}, {"error", e.Error}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, " %s=%s", kv[0], kv[1])
		}
	}
	if e.Text != "" {
		b.WriteString("\n" + indent(e.Text))
	}
	return b.String()
}

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }

// --- threads -------------------------------------------------------------------

type threadSum struct {
	Root      string    `json:"root"`
	FirstLine string    `json:"first_line"`
	Messages  int       `json:"messages"`
	Last      time.Time `json:"last_activity"`
	Runs      []string  `json:"runs,omitempty"`
	Contracts []string  `json:"contracts,omitempty"`
	seen      map[string]bool
}

// summarize groups the transcript by thread root (thread_ts, else ts) and
// keeps threads active since the given time, most recent first.
func summarize(entries []entry, since time.Time) []*threadSum {
	by := map[string]*threadSum{}
	for _, e := range entries {
		root := e.ThreadTS
		if root == "" {
			root = e.TS
		}
		at, err := time.Parse(time.RFC3339Nano, e.LoggedAt)
		if root == "" || err != nil {
			continue // a canvas has no thread
		}
		s := by[root]
		if s == nil {
			s = &threadSum{Root: root, seen: map[string]bool{}}
			by[root] = s
		}
		if at.After(s.Last) {
			s.Last = at
		}
		if message := e.Kind != "reaction" && e.Kind != "button" && e.Kind != "chat_routed"; message && e.TS != "" && !s.seen[e.TS] {
			s.seen[e.TS] = true
			s.Messages++
			if e.TS == root {
				s.FirstLine, _, _ = strings.Cut(e.Text, "\n")
			}
		}
		s.Runs = addUniq(s.Runs, append([]string{e.RunID}, e.Delivered...)...)
		s.Contracts = addUniq(s.Contracts, e.ContractID)
	}
	var out []*threadSum
	for _, s := range by {
		if !s.Last.Before(since) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

func printThreads(w io.Writer, sums []*threadSum, asJSON bool) error {
	if asJSON {
		if sums == nil {
			sums = []*threadSum{}
		}
		return json.NewEncoder(w).Encode(sums)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROOT\tLAST\tMSGS\tRUNS\tCONTRACTS\tTEXT")
	for _, s := range sums {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", s.Root, s.Last.Local().Format("2006-01-02 15:04"), s.Messages,
			dash(strings.Join(s.Runs, ",")), dash(strings.Join(s.Contracts, ",")), s.FirstLine)
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func addUniq(l []string, ss ...string) []string {
	for _, s := range ss {
		if s != "" && !contains(l, s) {
			l = append(l, s)
		}
	}
	return l
}

// --- thread --------------------------------------------------------------------

// printThread prints a thread of the owner DM as Slack has it now, oldest
// first, each message with the contract and run ids the transcript links to it.
func (a *adapter) printThread(ctx context.Context, w io.Writer, ts string, asJSON bool) error {
	msgs, err := a.replies(ctx, ts)
	if err != nil {
		return err
	}
	if asJSON {
		if msgs == nil {
			msgs = []slack.Message{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(msgs)
	}
	entries, err := readTranscript(a.transcriptPath())
	if err != nil {
		return err
	}
	runs, contracts := map[string][]string{}, map[string][]string{}
	for _, e := range entries {
		runs[e.TS] = addUniq(runs[e.TS], append([]string{e.RunID}, e.Delivered...)...)
		contracts[e.TS] = addUniq(contracts[e.TS], e.ContractID)
	}
	for _, m := range msgs {
		var ids string
		if c := contracts[m.Timestamp]; len(c) > 0 {
			ids += " contract=" + strings.Join(c, ",")
		}
		if r := runs[m.Timestamp]; len(r) > 0 {
			ids += " run=" + strings.Join(r, ",")
		}
		fmt.Fprintf(w, "%s %-5s %s%s\n%s\n", tsTime(m.Timestamp).Local().Format("2006-01-02 15:04:05"), a.who(m), m.Timestamp, ids, indent(m.Text))
	}
	return nil
}

func (a *adapter) who(m slack.Message) string {
	if m.User == a.owner && m.BotID == "" {
		return "owner"
	}
	return "bot"
}

// tsTime reads a Slack ts ("1712345678.123456").
func tsTime(ts string) time.Time {
	sec, frac, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return time.Unix(s, us*1000)
}
