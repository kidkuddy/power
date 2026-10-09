package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/slack-go/slack"
)

// Conversations other than the owner's DM threads: each DM with a coworker,
// each channel's top level and each channel thread is one --conv-workflow run
// holding one Claude session. It is told up front what kind of conversation it
// is in, wakes on every message there (slack.message signal, keyed by the
// conversation), and decides itself whether to talk, react, send a picture or
// stay quiet.

type convMsg struct {
	Text  string `json:"text"`
	TS    string `json:"ts"`
	From  string `json:"from"`
	Name  string `json:"name"`
	Owner bool   `json:"owner,omitempty"`
}

// convOf is a message's conversation: its key, its kind (dm | group), where
// replies go (a thread root, or "" for the conversation itself) and how to
// describe it to POWER.
func (a *adapter) convOf(m coMsg) (key, kind, reply, place string) {
	switch {
	case m.chType == "im":
		return "dm:" + m.channel, "dm", "", "a private DM between you and " + a.name(context.Background(), m.user)
	case m.thread != "":
		return "thr:" + m.channel + ":" + m.thread, "group", m.thread, "a thread in a Slack channel, several people can talk"
	}
	return "chan:" + m.channel, "group", "", "a Slack channel's main conversation, several people can talk"
}

// converse hands the message to its conversation's session: a signal when the
// session is waiting, a new run when there is none, the queue while it is busy.
func (a *adapter) converse(ctx context.Context, m coMsg) error {
	key, _, _, _ := a.convOf(m)
	a.st.Convs[key] = append(a.st.Convs[key], convMsg{Text: unslack(m.text), TS: m.ts, From: m.user, Name: a.name(ctx, m.user), Owner: m.user == a.owner})
	if err := a.save(); err != nil {
		return err
	}
	return a.routeConv(ctx, key, m)
}

// routeConvs retries every conversation with waiting messages.
func (a *adapter) routeConvs(ctx context.Context) {
	for key, q := range a.st.Convs {
		if len(q) == 0 {
			delete(a.st.Convs, key)
			continue
		}
		a.logErr(a.routeConv(ctx, key, coMsgOf(key, q[len(q)-1])))
	}
}

// coMsgOf rebuilds enough of a message from its key to route it again.
func coMsgOf(key string, q convMsg) coMsg {
	parts := strings.SplitN(key, ":", 3)
	m := coMsg{user: q.From, text: q.Text, ts: q.TS, channel: parts[1], chType: "channel"}
	if parts[0] == "dm" {
		m.chType = "im"
	}
	if parts[0] == "thr" && len(parts) == 3 {
		m.thread = parts[2]
	}
	return m
}

func (a *adapter) routeConv(ctx context.Context, key string, m coMsg) error {
	q := a.st.Convs[key]
	if len(q) == 0 {
		return nil
	}
	last := q[len(q)-1]
	text := last.Text
	if len(q) > 1 { // several arrived while she was busy: one turn, labeled
		var lines []string
		for _, x := range q {
			lines = append(lines, x.Name+": "+x.Text)
		}
		text = strings.Join(lines, "\n")
	}
	_, kind, reply, place := a.convOf(m)
	data := map[string]any{"text": text, "ts": last.TS, "from": last.From, "from_name": last.Name, "owner_msg": last.Owner}
	var sig struct {
		Delivered []string `json:"delivered"`
	}
	if err := a.kamiCall("POST", "/signal", map[string]any{"kekkai": a.chatKekkai, "type": "slack.message", "key": key, "data": data}, &sig); err != nil {
		return err
	}
	routed := entry{Dir: "in", Kind: "chat_routed", Channel: m.channel, TS: last.TS, ThreadTS: reply, User: last.From, Text: text, Workflow: a.convWorkflow}
	if len(sig.Delivered) > 0 {
		routed.Routed, routed.Delivered = "signalled", sig.Delivered
		a.record(routed)
		delete(a.st.Convs, key)
		return a.save()
	}
	var runs []struct {
		Workflow string `json:"workflow"`
	}
	v := url.Values{"status": {"active"}, "kekkai": {a.chatKekkai}, "var": {"kami_key=" + key}}
	if err := a.kamiCall("GET", "/runs?"+v.Encode(), nil, &runs); err != nil {
		return err
	}
	for _, r := range runs {
		if r.Workflow == a.convWorkflow {
			return nil // busy thinking: stays queued for the next tick
		}
	}
	// kami_*: where this session speaks and what it may remember; inputs only, nothing can change them later
	// kami_key: what it listens on and which session it resumes; like every kami_* input, nothing can change it later
	in := map[string]any{"kami_conv": kind, "kami_channel": m.channel, "kami_reply_ts": reply, "kami_key": key, "place": place,
		"history": a.convHistory(ctx, m, q), "kami_sessions": a.lastSessions(a.convWorkflow, "kami_key", key)}
	if kind == "dm" {
		in["kami_peer"] = m.user
	}
	for k, x := range data {
		in[k] = x
	}
	var run struct {
		ID string `json:"id"`
	}
	err := a.kamiCall("POST", "/runs", map[string]any{"kekkai": a.chatKekkai, "workflow": a.convWorkflow, "inputs": in}, &run)
	var ke *kamiErr
	if err != nil && !errors.As(err, &ke) {
		return err // kami unreachable: keep the queue
	}
	routed.Routed, routed.RunID = "started", run.ID
	if ke != nil {
		routed.Error = ke.Message
	}
	a.record(routed)
	delete(a.st.Convs, key)
	return a.save()
}

// ponytail: the last 20 messages; a longer conversation's start is dropped.
const convHistory = 20

// convHistory is the conversation before the queued messages as "name: text"
// lines, so a new session knows who said what. Empty when Slack will not
// show it (channels need channels:history / groups:history).
func (a *adapter) convHistory(ctx context.Context, m coMsg, q []convMsg) string {
	skip := map[string]bool{}
	for _, x := range q {
		skip[x.TS] = true
	}
	var msgs []slack.Message
	var err error
	if m.thread != "" {
		msgs, err = a.repliesIn(ctx, m.channel, m.thread)
	} else {
		var h *slack.GetConversationHistoryResponse
		h, err = a.api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{ChannelID: m.channel, Limit: convHistory + len(q)})
		if err == nil {
			msgs = h.Messages
			for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 { // newest first → oldest first
				msgs[i], msgs[j] = msgs[j], msgs[i]
			}
		}
	}
	if err != nil {
		a.logErr(fmt.Errorf("conversation history %s: %w", m.channel, err))
		return ""
	}
	var lines []string
	for _, x := range msgs {
		if skip[x.Timestamp] {
			continue
		}
		who := "POWER (you)"
		if x.BotID == "" {
			who = a.name(ctx, x.User)
		}
		lines = append(lines, who+": "+unslack(x.Text))
	}
	if len(lines) > convHistory {
		lines = lines[len(lines)-convHistory:]
	}
	return strings.Join(lines, "\n")
}
