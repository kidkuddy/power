package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/slack-go/slack"
)

// Coworkers: anyone but the owner in a DM, and anyone at all in a channel.
// A GitLab MR link gets review / just-approve buttons; the tapper's choice
// starts --mr-workflow. Anything else goes to that conversation's own Claude
// session (conv.go).

// Only gitlab.com, the host glab acts on: a link to any other host must not
// become an approval of the same path on gitlab.com. Same pattern as mr-open's MR_URL.
var mrURL = regexp.MustCompile(`https://gitlab\.com/[A-Za-z0-9][A-Za-z0-9._/-]*/-/merge_requests/\d+\b`)

const mrPrefix = "power:" // action ids power:review and power:approve

// mrAsk travels in the button value, so a tap needs no adapter state.
type mrAsk struct {
	URL       string `json:"url"`
	Requester string `json:"requester"`
	Channel   string `json:"channel"`
	Thread    string `json:"thread"`
}

type coMsg struct{ user, text, channel, ts, thread, chType string } // chType: im, channel, group, mpim

// fresh reports whether channel/ts is new, remembering it. A mention in a
// channel POWER is in arrives as both app_mention and message.
// ponytail: forgets everything past 500 entries; a duplicate that late is not one.
func (a *adapter) fresh(channel, ts string) bool {
	k := channel + "/" + ts
	if a.recent[k] {
		return false
	}
	if a.recent == nil || len(a.recent) > 500 {
		a.recent = map[string]bool{}
	}
	a.recent[k] = true
	return true
}

func (a *adapter) onCoworker(ctx context.Context, m coMsg) error {
	if !a.fresh(m.channel, m.ts) {
		return nil
	}
	a.record(entry{Dir: "in", Kind: "coworker", Channel: m.channel, TS: m.ts, ThreadTS: m.thread, User: m.user, Text: m.text})
	root := m.thread
	if root == "" {
		root = m.ts
	}
	out := entry{Kind: "coworker", Channel: m.channel, ThreadTS: root}
	if u := mrURL.FindString(unslack(m.text)); u != "" {
		val, _ := json.Marshal(mrAsk{URL: u, Requester: m.user, Channel: m.channel, Thread: root})
		out.Text = fmt.Sprintf("<@%s> an MR? For POWER?! Fine. Shall the great POWER *review* it properly, or just slap an *approve* on it?", m.user)
		btn := func(what, label string) *slack.ButtonBlockElement {
			return slack.NewButtonBlockElement(mrPrefix+what, string(val), slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
		}
		blocks := []slack.Block{section(out.Text), slack.NewActionBlock("power-mr", btn("review", "review it"), btn("approve", "just approve"))}
		_, _, err := a.post(ctx, out, blocks)
		return err
	}
	if m.user != a.owner {
		today := time.Now().Format("2006-01-02")
		if a.st.Day != today {
			a.st.Day, a.st.Banter = today, map[string]int{}
		}
		a.st.Banter[m.user]++
		if err := a.save(); err != nil {
			return err
		}
		if a.st.Banter[m.user] > banterPerDay { // every message is a Claude turn
			out.Text = "i only answer to the owner, bruh"
			_, _, err := a.post(ctx, out, nil)
			return err
		}
	}
	return a.converse(ctx, m)
}

// ponytail: a flat daily cap per coworker (every message is a Claude turn); per-hour buckets if 20 proves too blunt.
const banterPerDay = 20

// onMRTap starts the MR workflow for the coworker who asked (or the owner),
// and redraws the question without its buttons.
func (a *adapter) onMRTap(ctx context.Context, user, channel, msgTS, action, value string) error {
	var q mrAsk
	in := entry{Dir: "in", Kind: "button", Channel: channel, TS: msgTS, User: user, Text: action, Value: value, Workflow: a.mrWorkflow}
	if err := json.Unmarshal([]byte(value), &q); err != nil || (action != "review" && action != "approve") {
		in.Error = "bad MR button"
		a.record(in)
		return nil
	}
	if user != q.Requester && user != a.owner {
		in.Error = "not the requester: ignored"
		a.record(in)
		return nil
	}
	// who pressed, so mr-approve can refuse an author approving their own MR
	var name, email string
	if u, err := a.api.GetUserInfoContext(ctx, user); err == nil {
		name, email = u.RealName, u.Profile.Email
	}
	var run struct {
		ID string `json:"id"`
	}
	err := a.kamiCall("POST", "/runs", map[string]any{"kekkai": a.chatKekkai, "workflow": a.mrWorkflow,
		"inputs": map[string]any{"url": q.URL, "action": action, "channel": q.Channel, "reply_ts": q.Thread, "requester": q.Requester,
			"requester_name": name, "requester_email": email}}, &run)
	in.RunID = run.ID
	var ke *kamiErr
	if errors.As(err, &ke) {
		in.Error = ke.Message
		a.record(in)
		_, _, err = a.post(ctx, entry{Kind: "error", Channel: q.Channel, ThreadTS: q.Thread, Error: ke.Message,
			Text: "Ugh, POWER could not start that: " + ke.Message}, nil)
		return err
	}
	if err != nil {
		in.Error = err.Error()
		a.record(in)
		return err
	}
	a.record(in)
	line := fmt.Sprintf("<@%s> picked *%s*. POWER is on it, be grateful!", user, map[string]string{"review": "review it", "approve": "just approve"}[action])
	if _, _, _, err := a.api.UpdateMessageContext(ctx, channel, msgTS, slack.MsgOptionText(line, false), slack.MsgOptionBlocks(section(line))); err != nil {
		a.logErr(fmt.Errorf("redraw MR question: %w", err)) // best-effort: the run started
	}
	return nil
}

// isCoworker reports whether a message goes to the coworker handler: any
// channel, or a DM from someone other than the owner.
func (a *adapter) isCoworker(channelType, user string) bool {
	return a.coworkers && (channelType != "im" || user != a.owner)
}

// name is a Slack user's display name ("the owner (owner)" for the owner),
// cached; the id when the lookup fails.
func (a *adapter) name(ctx context.Context, user string) string {
	if n, ok := a.names[user]; ok {
		return n
	}
	n := user
	if u, err := a.api.GetUserInfoContext(ctx, user); err == nil {
		n = u.Profile.DisplayName
		if n == "" {
			n = u.RealName
		}
	}
	if user == a.owner {
		n += " (owner)"
	}
	if a.names == nil {
		a.names = map[string]string{}
	}
	a.names[user] = n
	return n
}
