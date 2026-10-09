package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"kami/internal/eventbus"
)

// A status card is one message per top-level run (a goal), the first reply in
// its thread, edited in place: one line per milestone, one line per
// child run that follows the child's latest milestone. Decisions, problems
// and the run's result still post as their own messages.
type card struct {
	TS       string         `json:"ts"`
	Thread   string         `json:"thread"`
	Title    string         `json:"title"`
	Status   string         `json:"status"` // running | ✅ done | ❌ <status>
	Lines    []string       `json:"lines"`
	Children map[string]int `json:"children,omitempty"` // child run id -> its line
}

// ponytail: keeps the last 30 lines; a 3000-char section fits that comfortably.
const cardLines = 30

func (c *card) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "*POWER's status card* · %s · *%s*\n", c.Title, c.Status)
	lines := c.Lines
	if len(lines) > cardLines {
		lines = append([]string{"…"}, lines[len(lines)-cardLines:]...)
	}
	for _, l := range lines {
		b.WriteString("• " + l + "\n")
	}
	fmt.Fprintf(&b, "_updated %s_", time.Now().Format("15:04"))
	return b.String()
}

// onCard records one milestone of the run tree rooted at rootID on its card,
// posting and pinning the card on the first one.
func (a *adapter) onCard(ctx context.Context, rootID string, root link, ev eventbus.Event, workflow, text string) error {
	c := a.st.Cards[rootID]
	if c == nil {
		c = &card{Thread: root.thread, Title: fmt.Sprintf("%s `%s`", root.workflow, rootID), Status: "running"}
	}
	if c.Children == nil { // omitted from the state file while it was empty
		c.Children = map[string]int{}
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
		if i, ok := c.Children[ev.Run]; ok {
			c.Lines[i] = line
		} else {
			c.Children[ev.Run] = len(c.Lines)
			c.Lines = append(c.Lines, line)
		}
	}
	t := c.text()
	e := entry{Dir: "out", Kind: "card", Channel: a.im, TS: c.TS, ThreadTS: c.Thread, Text: t, RunID: rootID, Workflow: root.workflow}
	if c.TS == "" {
		_, ts, err := a.post(ctx, e, []slack.Block{section(t)})
		if badThread(err) {
			return nil
		}
		if err != nil {
			return err
		}
		c.TS = ts
	} else {
		im, err := a.dm(ctx)
		if err == nil {
			_, _, _, err = a.api.UpdateMessageContext(ctx, im, c.TS, slack.MsgOptionText(t, false), slack.MsgOptionBlocks(section(t)))
		}
		if err != nil {
			e.Kind, e.Error = "error", "card: "+err.Error()
			a.record(e)
			return err
		}
		a.record(e)
	}
	a.st.Cards[rootID] = c
	return a.save()
}

// wrap unpins a thread's root and every card in it (kami-slack wrap).
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
	return true, a.onCard(ctx, l.root, root, eventbus.Event{Run: l.root, Type: "run.say"}, root.workflow, "🤖 AFK answered: "+answer)
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
