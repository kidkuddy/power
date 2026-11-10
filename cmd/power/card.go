package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"

	"power/internal/kami"
)

// A status card is one message per top-level run (a goal), the first reply in
// its thread, edited in place: one line per milestone, one line per
// child run that follows the child's latest milestone. Decisions, problems
// and the run's result still post as their own messages.
type card struct {
	TS       string          `json:"ts"`
	Thread   string          `json:"thread"`
	Title    string          `json:"title"`
	Status   string          `json:"status"` // running | ✅ done | ❌ <status>
	Lines    []string        `json:"lines"`
	Children map[string]int  `json:"children,omitempty"` // child run id -> its line
	Ended    map[string]bool `json:"ended,omitempty"`    // child run id -> it finished done (true) or not (false)
}

// ponytail: keeps at most the last 30 lines, fewer when they do not fit the message.
const (
	cardLines = 30
	cardLine  = 200 // runes: a milestone can carry a whole report
)

type row struct {
	text   string
	status slack.TaskCardStatus
}

// rows is the card's lines, each cut to cardLine runes, a run of identical lines
// shown once with ×n. A root milestone is complete; a child is in progress until it ends.
func (c *card) rows() []row {
	child := map[int]string{}
	for id, i := range c.Children {
		child[i] = id
	}
	var rows []row
	n := 0
	for i, l := range c.Lines {
		if n++; i+1 < len(c.Lines) && c.Lines[i+1] == l {
			continue
		}
		if r := []rune(l); len(r) > cardLine {
			l = string(r[:cardLine-1]) + "…"
		}
		if n > 1 {
			l += fmt.Sprintf(" ×%d", n)
		}
		st := slack.TaskCardStatusComplete
		if id, ok := child[i]; ok {
			if done, ended := c.Ended[id]; !ended {
				st = slack.TaskCardStatusInProgress
			} else if !done {
				st = slack.TaskCardStatusError
			}
		}
		rows, n = append(rows, row{l, st}), 0
	}
	return rows
}

// plan is the card as Slack's plan block: one task per row, the last cardLines
// (a plan takes at most 50 tasks). The text card stays as the notification fallback.
func (c *card) plan() slack.Block {
	rows := c.rows()
	rows = rows[max(0, len(rows)-cardLines):]
	tasks := make([]*slack.TaskCardBlock, len(rows))
	for i, r := range rows {
		tasks[i] = slack.NewTaskCardBlock(fmt.Sprint(i), r.text).WithStatus(r.status)
	}
	return slack.NewPlanBlock(fmt.Sprintf("POWER's status card · %s · %s", c.Title, c.Status)).WithTasks(tasks...)
}

// text is the card within limit, as size measures it: rows, the oldest dropped until it fits.
func (c *card) text(limit int, size func(string) int) string {
	var lines []string
	for _, r := range c.rows() {
		lines = append(lines, r.text)
	}
	head := fmt.Sprintf("*POWER's status card* · %s · *%s*\n", c.Title, c.Status)
	foot := fmt.Sprintf("_updated %s_", time.Now().Format("15:04"))
	for drop := max(0, len(lines)-cardLines); ; drop++ {
		var b strings.Builder
		b.WriteString(head)
		if drop > 0 {
			b.WriteString("• …\n")
		}
		for _, l := range lines[drop:] {
			b.WriteString("• " + l + "\n")
		}
		b.WriteString(foot)
		if size(b.String()) <= limit || drop == len(lines) {
			return b.String()
		}
	}
}

// onCard records one milestone of the run tree rooted at rootID on its card,
// posting and pinning the card on the first one.
func (a *adapter) onCard(ctx context.Context, rootID string, root link, ev kami.Event, workflow, text string) error {
	c := &card{Thread: root.thread, Title: fmt.Sprintf("%s `%s`", root.workflow, rootID), Status: "running"}
	if old := a.st.Cards[rootID]; old != nil { // a copy: the stored card changes only once Slack has the new one
		*c = *old
		c.Lines, c.Children, c.Ended = slices.Clone(old.Lines), maps.Clone(old.Children), maps.Clone(old.Ended)
	}
	if c.Children == nil { // omitted from the state file while it was empty
		c.Children = map[string]int{}
	}
	if c.Ended == nil {
		c.Ended = map[string]bool{}
	}
	switch {
	case ev.Run == rootID && ev.Type == "run.finished":
		if s, _ := ev.Data["status"].(string); s == "done" {
			c.Status = "✅ done"
		} else {
			c.Status = "❌ " + s
		}
	case ev.Run == rootID:
		c.Lines = append(c.Lines, text)
	default: // a child (or grandchild): one line, overwritten
		line := fmt.Sprintf("↳ %s `%s`: %s", workflow, ev.Run, strings.TrimPrefix(text, "↳ POWER started "+workflow))
		if ev.Type == "run.started" {
			line = fmt.Sprintf("↳ %s `%s`: started", workflow, ev.Run)
		}
		if ev.Type == "run.finished" {
			c.Ended[ev.Run] = ev.Data["status"] == kami.StDone
		}
		if i, ok := c.Children[ev.Run]; ok {
			c.Lines[i] = line
		} else {
			c.Children[ev.Run] = len(c.Lines)
			c.Lines = append(c.Lines, line)
		}
	}
	limit, size := 3000, utf8.RuneCountInString // a section's text
	sealed := a.e2e != nil && a.isSealed(c.Thread)
	if sealed {
		limit, size = sealedMax(a.sealedForm(c.Thread)), func(s string) int { return len(s) }
	}
	t := c.text(limit, size)
	e := entry{Dir: "out", Kind: "card", Channel: a.im, TS: c.TS, ThreadTS: c.Thread, Text: t, RunID: rootID, Workflow: root.workflow}
	// a sealed thread keeps the text card: sealOut would drop a plan block, and its titles are plaintext
	err := error(nil)
	if !sealed {
		err = a.putCard(ctx, c, e, c.plan())
	}
	if sealed || badBlocks(err) { // ponytail: Slack refusing the plan falls back to the text card
		err = a.putCard(ctx, c, e, section(t))
	}
	if err != nil { // the error entry is the record; a card never holds up the events behind it
		a.logErr(fmt.Errorf("card %s: %w", rootID, err))
		return nil
	}
	a.st.Cards[rootID] = c
	return a.save()
}

// badBlocks is Slack refusing a message's blocks (not its channel or thread).
func badBlocks(err error) bool {
	var se slack.SlackErrorResponse
	return errors.As(err, &se) && strings.HasPrefix(se.Err, "invalid_blocks")
}

// putCard posts c (setting c.TS) or edits it in place with block.
func (a *adapter) putCard(ctx context.Context, c *card, e entry, block slack.Block) error {
	if c.TS == "" {
		_, ts, err := a.post(ctx, e, []slack.Block{block})
		c.TS = ts
		return err
	}
	im, err := a.dm(ctx)
	if err == nil {
		var st string
		var blocks []slack.Block
		if st, blocks, _, err = a.sealOut(ctx, im, c.Thread, e.RunID, e.Text, []slack.Block{block}); err == nil {
			_, _, _, err = a.api.UpdateMessageContext(ctx, im, c.TS, slack.MsgOptionText(st, false), slack.MsgOptionBlocks(blocks...))
		}
	}
	if err != nil {
		e.Kind, e.Error = "error", "card: "+err.Error()
	}
	a.record(e)
	return err
}

// status shows what a run's worker is doing under its thread (Slack's "POWER is …"
// line), cleared when the worker exits. Cosmetic: a failure is logged, never retried.
// ponytail: one status per thread, so a child exiting clears a sibling's; per-run
// tracking if parallel children in one thread become common.
func (a *adapter) status(ctx context.Context, ev kami.Event) {
	if ev.Run == "" || (!ev.TS.IsZero() && time.Since(ev.TS) > staleMilestone) {
		return
	}
	l, err := a.runLink(ev.Run, 0)
	// as sealOut: a sealed thread, or any thread of a sealed kekkai's run, shows nothing in plaintext
	if err != nil || l.thread == "" || (a.e2e != nil && (a.isSealed(l.thread) || a.e2e.kekkai[l.kekkai])) {
		a.logErr(err)
		return
	}
	text := ""
	if ev.Type == "worker.started" {
		state, _ := ev.Data["state"].(string)
		if text = fmt.Sprintf("is running %s · %s…", l.workflow, state); l.workflow == a.chatWorkflow {
			text = "is thinking…"
		}
	}
	im, err := a.dm(ctx)
	if err == nil {
		err = a.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{ChannelID: im, ThreadTS: l.thread, Status: text})
	}
	if err != nil {
		a.logErr(fmt.Errorf("status %s: %w", ev.Run, err))
	}
}

// wrap unpins a thread's root and every card in it (power wrap).
func (a *adapter) wrap(ctx context.Context, thread string) error {
	if err := a.load(); err != nil {
		return err
	}
	err := a.pin(ctx, "", thread, false)
	for _, c := range a.st.Cards {
		if c.Thread == thread && c.TS != "" {
			if e := a.pin(ctx, "", c.TS, false); e != nil {
				err = e
			}
		}
	}
	return err
}

// afkOnCard puts an AFK resolver's answer on its run tree's card instead of a
// message of its own; false when there is no card (or a human answered).
func (a *adapter) afkOnCard(ctx context.Context, t *thread, by, answer string) (bool, error) {
	if by != "resolver" || t.RunID == "" {
		return false, nil
	}
	l, err := a.runLink(t.RunID, 0)
	if err != nil || l.root == "" { // the run is gone
		return false, err
	}
	root := l
	if l.root != t.RunID {
		if root, err = a.runLink(l.root, 0); err != nil {
			return false, err
		}
	}
	if a.st.Cards[l.root] == nil || root.workflow == a.chatWorkflow {
		return false, nil
	}
	if r := []rune(firstLineOf(answer)); len(r) > 120 {
		answer = string(r[:119]) + "…"
	} else {
		answer = string(r)
	}
	return true, a.onCard(ctx, l.root, root, kami.Event{Run: l.root, Type: "run.say"}, root.workflow, "🤖 AFK answered: "+answer)
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
