package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"kami/internal/engine"
	"kami/internal/eventbus"
)

// serve runs the loop. Socket Mode delivers DMs and button taps down an
// outbound websocket (no public endpoint); kami events are polled. One
// goroutine handles both, so the state needs no lock.
// ponytail: polls /events?since= instead of holding follow=1 open; latency is
// one poll interval and a daemon restart needs no reconnect logic.
func (a *adapter) serve(ctx context.Context, poll time.Duration) error {
	if err := a.load(); err != nil {
		return err
	}
	sm := socketmode.New(a.api)
	errc := make(chan error, 1)
	go func() { errc <- sm.RunContext(ctx) }()
	log.Printf("kami-slack: serving; state %s, from event %d", a.statePath(), a.st.Since)
	t := time.NewTicker(poll)
	defer t.Stop()
	a.logErr(a.events(ctx))
	for {
		select {
		case err := <-errc:
			return fmt.Errorf("socket mode: %w", err)
		case <-t.C:
			a.logErr(a.events(ctx))
			a.routeAll(ctx)
		case evt := <-sm.Events:
			switch evt.Type {
			case socketmode.EventTypeConnected:
				log.Printf("kami-slack: connected to Slack")
			case socketmode.EventTypeEventsAPI:
				ev, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok || evt.Request == nil {
					continue
				}
				sm.Ack(*evt.Request)
				if m, ok := ev.InnerEvent.Data.(*slackevents.MessageEvent); ok {
					a.logErr(a.onMessage(ctx, m))
				}
			case socketmode.EventTypeInteractive:
				cb, ok := evt.Data.(slack.InteractionCallback)
				if !ok || evt.Request == nil {
					continue
				}
				sm.Ack(*evt.Request)
				a.logErr(a.onAction(ctx, cb))
			}
		}
	}
}

func (a *adapter) logErr(err error) {
	if err != nil {
		log.Printf("kami-slack: %v", err)
	}
}

// --- kami → Slack --------------------------------------------------------------

func (a *adapter) events(ctx context.Context) error {
	// every type: the filter takes one prefix and milestones are run.* events
	q := url.Values{"since": {fmt.Sprint(a.st.Since)}}
	if a.kekkai != "" {
		q.Set("kekkai", a.kekkai)
	}
	resp, err := a.kami.Get("http://kami/events?" + q.Encode())
	if err != nil {
		return fmt.Errorf("kami: %v", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	dirty := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue // keepalive
		}
		var ev struct {
			eventbus.Event
			Error string `json:"error"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("kami event: %v", err)
		}
		if ev.Error != "" {
			return fmt.Errorf("kami events: %s", ev.Error)
		}
		// A failed post returns without advancing Since: the event is retried next poll.
		if err := a.handle(ctx, ev.Event); err != nil {
			a.logErr(a.save())
			return err
		}
		a.st.Since = ev.ID
		if !followed(ev.Type) {
			dirty = true // saved once per poll: most events are not ours
			continue
		}
		dirty = false
		if err := a.save(); err != nil {
			return err
		}
	}
	if dirty {
		if err := a.save(); err != nil {
			return err
		}
	}
	return sc.Err()
}

func followed(typ string) bool {
	return strings.HasPrefix(typ, "contract.") || typ == "run.say" || typ == "run.started" || typ == "run.finished"
}

// ponytail: a milestone older than this is history being replayed (the adapter
// was down, or upgraded from a contract-only cursor) and is not posted.
const staleMilestone = 10 * time.Minute

// milestone posts run.say, a child's run.started and run.finished into the
// run's thread, one line each. A run with no thread gets nothing.
func (a *adapter) milestone(ctx context.Context, ev eventbus.Event) error {
	if ev.Run == "" || (!ev.TS.IsZero() && time.Since(ev.TS) > staleMilestone) {
		return nil
	}
	if p, _ := ev.Data["parent"].(string); ev.Type == "run.started" && p == "" {
		return nil // a top-level run's thread is where it was started from
	}
	l, err := a.runLink(ev.Run, 0)
	if err != nil || l.thread == "" {
		return err
	}
	var text string
	switch ev.Type {
	case "run.say":
		text, _ = ev.Data["text"].(string)
	case "run.started":
		text = "↳ started " + l.workflow
	case "run.finished":
		status, _ := ev.Data["status"].(string)
		report, _ := ev.Data["report"].(string)
		if status == engine.StDone {
			text = "✅ " + l.workflow
		} else {
			text = "❌ " + l.workflow + " " + status
		}
		if report = strings.TrimSpace(report); report != "" {
			text += ": " + report
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	_, _, err = a.post(ctx, entry{Kind: "milestone", ThreadTS: l.thread, Text: text, RunID: ev.Run, Workflow: l.workflow}, nil)
	if badThread(err) {
		return nil // a thread_ts that is not a thread of the owner DM: the error entry is the record
	}
	return err
}

// badThread is Slack refusing a thread_ts. Retrying cannot fix it, and a
// failed event blocks every event after it.
func badThread(err error) bool {
	var se slack.SlackErrorResponse
	return errors.As(err, &se) && (se.Err == "invalid_thread_ts" || se.Err == "thread_not_found")
}

type link struct{ thread, workflow string }

// runLink finds a run's Slack thread: its var thread_ts, else its parent's,
// up the chain. Only runs with a thread are cached: a run may set thread_ts later.
func (a *adapter) runLink(id string, depth int) (link, error) {
	if l, ok := a.links[id]; ok {
		return l, nil
	}
	var r struct {
		Workflow string         `json:"workflow"`
		Parent   string         `json:"parent"`
		Vars     map[string]any `json:"vars"`
	}
	err := a.kamiCall("GET", "/runs/"+url.PathEscape(id), nil, &r)
	var ke *kamiErr
	if errors.As(err, &ke) {
		return link{}, nil // gone: no thread
	}
	if err != nil {
		return link{}, err
	}
	l := link{workflow: r.Workflow}
	l.thread, _ = r.Vars["thread_ts"].(string)
	if l.thread == "" && r.Parent != "" && depth < 32 {
		p, err := a.runLink(r.Parent, depth+1)
		if err != nil {
			return link{}, err
		}
		l.thread = p.thread
	}
	if l.thread != "" {
		if a.links == nil {
			a.links = map[string]link{}
		}
		a.links[id] = l
	}
	return l, nil
}

// threadRuns is the ids of the runs tied to a thread: those with thread_ts = key,
// and the children seen there.
func (a *adapter) threadRuns(key string) (string, error) {
	var runs []struct {
		ID string `json:"id"`
	}
	if err := a.kamiCall("GET", "/runs?"+url.Values{"var": {"thread_ts=" + key}}.Encode(), nil, &runs); err != nil {
		return "", err
	}
	var ids []string
	for _, r := range runs {
		ids = addUniq(ids, r.ID)
	}
	for id, l := range a.links {
		if l.thread == key {
			ids = addUniq(ids, id)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ","), nil
}

func (a *adapter) handle(ctx context.Context, ev eventbus.Event) error {
	if !followed(ev.Type) {
		return nil
	}
	if !strings.HasPrefix(ev.Type, "contract.") {
		return a.milestone(ctx, ev)
	}
	id, _ := ev.Data["id"].(string)
	t := a.st.Threads[id]
	if ev.Type != "contract.opened" && t == nil {
		return nil // not one we posted
	}
	switch ev.Type {
	case "contract.opened":
		if t != nil {
			return nil
		}
		var c engine.ContractView
		if err := a.kamiCall("GET", "/contracts/"+url.PathEscape(id), nil, &c); err != nil {
			return err
		}
		if c.Status != "open" { // replaying history: decided while we were away
			return nil
		}
		text := render(&c)
		buttons := len(c.Options) > 0 && len(c.Fields) == 0
		l, err := a.runLink(c.RunID, 0)
		if err != nil {
			return err
		}
		blocks := contractBlocks(text, c.ID, c.Options, c.Default, buttons)
		e := entry{Kind: "contract", ThreadTS: l.thread, Text: text, ContractID: id, RunID: c.RunID, Workflow: c.Workflow}
		_, ts, err := a.post(ctx, e, blocks)
		if badThread(err) { // the run's thread_ts is not one of ours: top-level, as for a thread-less run
			l.thread, e.ThreadTS = "", ""
			_, ts, err = a.post(ctx, e, blocks)
		}
		if err != nil {
			return err
		}
		a.st.Threads[id] = &thread{TS: ts, Thread: l.thread, Text: text, Buttons: buttons, RunID: c.RunID, Workflow: c.Workflow}
		log.Printf("kami-slack: contract %s → DM %s", id, ts)
	case "contract.resolved":
		if !t.Mine {
			by, _ := ev.Data["by"].(string)
			ans, _ := ev.Data["answer"].(string)
			a.settle(ctx, id, t, fmt.Sprintf("answered *%s* by %s", ans, by))
			if _, _, err := a.post(ctx, t.note("followup", id, fmt.Sprintf("✓ resolved by %s → %s. No reply needed.", by, ans)), nil); err != nil {
				return err
			}
		}
		delete(a.st.Threads, id)
	case "contract.cancelled":
		a.settle(ctx, id, t, "cancelled")
		if _, _, err := a.post(ctx, t.note("followup", id, "✗ cancelled (the run moved on). No reply needed."), nil); err != nil {
			return err
		}
		delete(a.st.Threads, id)
	case "contract.resolver_failed":
		why, _ := ev.Data["error"].(string)
		_, _, err := a.post(ctx, t.note("followup", id, "⚠ the AFK resolver failed: "+why+"\nIt is back with you: answer above or reply here."), nil)
		return err
	case "contract.grace_expired":
		_, _, err := a.post(ctx, t.note("followup", id, "⏱ grace period over; the AFK resolver is answering now."), nil)
		return err
	}
	return nil
}

// settle redraws a button message without its buttons, so it reads as a
// record rather than a live question. Best-effort: the answer already landed.
func (a *adapter) settle(ctx context.Context, id string, t *thread, line string) {
	if !t.Buttons {
		return
	}
	im, err := a.dm(ctx)
	if err == nil {
		_, _, _, err = a.api.UpdateMessageContext(ctx, im, t.TS, slack.MsgOptionText(t.Text, false),
			slack.MsgOptionBlocks(section(t.Text), slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, line, false, false))))
	}
	if err != nil {
		a.logErr(err)
		e := t.note("error", id, line)
		e.ThreadTS, e.TS, e.Error = "", t.TS, "redraw: "+err.Error()
		a.record(e)
	}
}

// note is an outbound entry in a contract's thread: its run's thread, or for
// a top-level contract message the thread under that message.
func (t *thread) note(kind, id, text string) entry {
	root := t.Thread
	if root == "" {
		root = t.TS
	}
	return entry{Dir: "out", Kind: kind, ThreadTS: root, Text: text, ContractID: id, RunID: t.RunID, Workflow: t.Workflow}
}

// --- Slack → kami --------------------------------------------------------------

const actionPrefix = "kami:"

// onAction handles a button tap. Only the owner may resolve anything.
func (a *adapter) onAction(ctx context.Context, cb slack.InteractionCallback) error {
	if cb.Type != slack.InteractionTypeBlockActions {
		return nil
	}
	if cb.User.ID != a.owner {
		for _, act := range cb.ActionCallback.BlockActions {
			if rest, ok := strings.CutPrefix(act.ActionID, actionPrefix); ok {
				id, _, _ := strings.Cut(rest, ":")
				a.record(entry{Dir: "in", Kind: "button", Channel: cb.Channel.ID, TS: cb.Message.Timestamp, User: cb.User.ID,
					Text: act.Value, Value: act.Value, ContractID: id, Error: "not the owner: ignored"})
			}
		}
		return nil
	}
	for _, act := range cb.ActionCallback.BlockActions {
		rest, ok := strings.CutPrefix(act.ActionID, actionPrefix)
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(rest, ":")
		e := entry{Dir: "in", Kind: "button", Channel: cb.Channel.ID, TS: cb.Message.Timestamp, User: cb.User.ID,
			Text: act.Value, Value: act.Value, ContractID: id}
		t := a.st.Threads[id]
		if t == nil {
			log.Printf("kami-slack: tap on unknown contract %s", id)
			e.Error = "unknown contract"
			a.record(e)
			continue
		}
		e.TS, e.RunID, e.Workflow = t.TS, t.RunID, t.Workflow
		ans, err := a.resolve(ctx, id, t, act.Value, nil, false)
		e.Answer = ans
		if err != nil {
			e.Error = err.Error()
		}
		a.record(e)
		if err != nil {
			return err
		}
	}
	return nil
}

// onMessage handles an owner's DM: a reply in a contract's thread answers the
// contract; anything else goes to a chat run.
func (a *adapter) onMessage(ctx context.Context, m *slackevents.MessageEvent) error {
	// The bot's own posts come back as events; the transcript has them as "out".
	if m.ChannelType != "im" || m.BotID != "" || m.User == "" {
		return nil
	}
	inThread := m.ThreadTimeStamp != "" && m.ThreadTimeStamp != m.TimeStamp
	in := entry{Dir: "in", Kind: "dm", Channel: m.Channel, TS: m.TimeStamp, ThreadTS: m.ThreadTimeStamp, User: m.User, Text: m.Text}
	if inThread {
		in.Kind = "thread_reply"
	}
	if m.User != a.owner || (m.SubType != "" && m.SubType != "thread_broadcast") {
		in.Error = "ignored: not the owner or subtype " + m.SubType
		a.record(in)
		return nil
	}
	text := unslack(m.Text)
	answer := func(id string, t *thread, c *engine.ContractView) error {
		in.ContractID, in.RunID, in.Workflow = id, t.RunID, t.Workflow
		a.record(in)
		ans, data := parseReply(c, text)
		_, err := a.resolve(ctx, id, t, ans, data, true)
		return err
	}
	for id, t := range a.st.Threads { // the thread under a top-level contract message
		if !inThread || t.Thread != "" || t.TS != m.ThreadTimeStamp {
			continue
		}
		var c engine.ContractView
		if err := a.kamiCall("GET", "/contracts/"+url.PathEscape(id), nil, &c); err != nil {
			return err
		}
		return answer(id, t, &c)
	}
	if inThread { // a run's thread: the reply answers its one open contract that takes text
		var hit string
		var hitView *engine.ContractView
		n := 0
		for id, t := range a.st.Threads {
			if t.Thread != m.ThreadTimeStamp {
				continue
			}
			var c engine.ContractView
			if err := a.kamiCall("GET", "/contracts/"+url.PathEscape(id), nil, &c); err != nil {
				return err
			}
			if c.Contract != nil && c.Status == "open" && takesText(&c, text) {
				hit, hitView, n = id, &c, n+1
			}
		}
		if n == 1 {
			return answer(hit, a.st.Threads[hit], hitView)
		}
	}
	a.record(in)
	if a.chatWorkflow == "" || m.SubType != "" {
		return nil
	}
	key := m.TimeStamp // a top-level DM starts a thread under itself
	if inThread {
		key = m.ThreadTimeStamp
	}
	re := entry{Dir: "out", Kind: "reaction", Channel: m.Channel, TS: m.TimeStamp, ThreadTS: m.ThreadTimeStamp, Text: "eyes"}
	if err := a.api.AddReactionContext(ctx, "eyes", slack.NewRefToMessage(m.Channel, m.TimeStamp)); err != nil {
		log.Printf("kami-slack: react on %s: %v", m.TimeStamp, err)
		re.Error = err.Error()
	}
	a.record(re)
	a.st.Queue[key] = append(a.st.Queue[key], queued{Text: text, TS: m.TimeStamp})
	if err := a.save(); err != nil {
		return err
	}
	err := a.route(ctx, key)
	for _, q := range a.st.Queue[key] { // not handed over yet; later ticks log only the hand-over
		if q.TS == m.TimeStamp {
			e := a.routed("queued", key, q)
			if err != nil {
				e.Error = err.Error()
			}
			a.record(e)
		}
	}
	return err
}

func (a *adapter) routed(how, key string, q queued) entry {
	return entry{Dir: "in", Kind: "chat_routed", Channel: a.im, TS: q.TS, ThreadTS: key, Text: q.Text, Routed: how, Workflow: a.chatWorkflow}
}

// --- chat ----------------------------------------------------------------------

// routeAll retries every chat thread that still has queued messages.
func (a *adapter) routeAll(ctx context.Context) {
	for key := range a.st.Queue {
		a.logErr(a.route(ctx, key))
	}
}

// route hands a chat thread's queued messages to kami, oldest first: the head
// goes as a slack.message signal to a run waiting on this thread; while a chat
// run owns the thread but is busy the queue waits for the next tick; with no
// run at all, a new chat run starts with everything queued.
func (a *adapter) route(ctx context.Context, key string) error {
	q := a.st.Queue[key]
	if len(q) == 0 {
		delete(a.st.Queue, key)
		return nil
	}
	var sig struct {
		Delivered []string `json:"delivered"`
	}
	err := a.kamiCall("POST", "/signal", map[string]any{"kekkai": a.chatKekkai, "type": "slack.message", "key": key,
		"data": map[string]any{"text": q[0].Text, "ts": q[0].TS, "history": ""}}, &sig)
	if err != nil {
		return err
	}
	if len(sig.Delivered) > 0 {
		e := a.routed("signalled", key, q[0])
		e.Delivered = sig.Delivered
		a.record(e)
		a.pop(key, 1)
		return a.save()
	}
	var runs []struct {
		Workflow string `json:"workflow"`
	}
	v := url.Values{"status": {"active"}, "kekkai": {a.chatKekkai}, "var": {"thread_ts=" + key}}
	if err := a.kamiCall("GET", "/runs?"+v.Encode(), nil, &runs); err != nil {
		return err
	}
	for _, r := range runs {
		if r.Workflow == a.chatWorkflow {
			return nil // busy: stays queued
		}
	}
	var texts []string
	skip := map[string]bool{}
	for _, m := range q {
		texts = append(texts, m.Text)
		skip[m.TS] = true
	}
	history := ""
	if !skip[key] { // the thread existed before these messages
		if history, err = a.history(ctx, key, skip); err != nil {
			return err
		}
	}
	im, err := a.dm(ctx)
	if err != nil {
		return err
	}
	tied, err := a.threadRuns(key)
	if err != nil {
		return err
	}
	var run struct {
		ID string `json:"id"`
	}
	err = a.kamiCall("POST", "/runs", map[string]any{"kekkai": a.chatKekkai, "workflow": a.chatWorkflow,
		"inputs": map[string]any{"thread_ts": key, "channel": im, "text": strings.Join(texts, "\n\n"), "history": history, "thread_runs": tied}}, &run)
	var ke *kamiErr
	if err != nil && !errors.As(err, &ke) {
		return err // kami unreachable: keep the queue, retry next tick
	}
	if ke == nil {
		e := a.routed("started", key, queued{TS: q[0].TS, Text: strings.Join(texts, "\n\n")})
		e.RunID = run.ID
		a.record(e)
	}
	a.pop(key, len(q))
	if serr := a.save(); serr != nil {
		return serr
	}
	if ke != nil {
		_, _, err = a.post(ctx, entry{Kind: "error", ThreadTS: key, Text: "✗ could not start " + a.chatWorkflow + ": " + ke.Message,
			Workflow: a.chatWorkflow, Error: ke.Message}, nil)
		return err
	}
	return nil
}

func (a *adapter) pop(key string, n int) {
	if q := a.st.Queue[key][n:]; len(q) > 0 {
		a.st.Queue[key] = q
	} else {
		delete(a.st.Queue, key)
	}
}

const maxHistory = 8000

// history is the thread so far as "owner: …" / "bot: …" lines, oldest first,
// without the messages in skip, cut from the front to the last ~8000 bytes.
func (a *adapter) history(ctx context.Context, threadTS string, skip map[string]bool) (string, error) {
	msgs, err := a.replies(ctx, threadTS)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, m := range msgs {
		if !skip[m.Timestamp] {
			lines = append(lines, a.who(m)+": "+unslack(m.Text))
		}
	}
	h := strings.Join(lines, "\n")
	if len(h) > maxHistory {
		h = h[len(h)-maxHistory:]
		if i := strings.IndexByte(h, '\n'); i >= 0 {
			h = h[i+1:] // drop the partial first line
		}
		h = strings.ToValidUTF8(h, "")
	}
	return h, nil
}

// replies is a thread of the owner DM, oldest first, every page.
func (a *adapter) replies(ctx context.Context, threadTS string) ([]slack.Message, error) {
	im, err := a.dm(ctx)
	if err != nil {
		return nil, err
	}
	var out []slack.Message
	p := &slack.GetConversationRepliesParameters{ChannelID: im, Timestamp: threadTS, Limit: 200}
	for {
		msgs, more, cursor, err := a.api.GetConversationRepliesContext(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("conversations.replies %s: %w", threadTS, err)
		}
		out = append(out, msgs...)
		if !more || cursor == "" {
			return out, nil
		}
		p.Cursor = cursor
	}
}

// resolve answers a contract as the owner and reports back in Slack: a button
// message is redrawn with the answer; a thread reply gets a reply; a refusal
// from kami goes to the thread so the owner can retry. It returns the answer
// kami recorded, empty when kami refused.
func (a *adapter) resolve(ctx context.Context, id string, t *thread, answer string, data map[string]any, inThread bool) (string, error) {
	body := map[string]any{"answer": answer, "data": data, "note": "via slack (kami-slack)"}
	var out engine.ContractView
	err := a.kamiCall("POST", "/contracts/"+url.PathEscape(id)+"/resolve", body, &out)
	var ke *kamiErr
	if errors.As(err, &ke) {
		e := t.note("error", id, "✗ "+ke.Message+"\nTry again.")
		e.Error = ke.Message
		_, _, err = a.post(ctx, e, nil)
		return "", err
	}
	if err != nil {
		return "", err
	}
	t.Mine = true
	if err := a.save(); err != nil {
		return "", err
	}
	if out.Resolution != nil {
		answer = out.Resolution.Answer
	}
	a.settle(ctx, id, t, fmt.Sprintf("answered *%s* by <@%s>", answer, a.owner))
	if inThread {
		_, _, err = a.post(ctx, t.note("followup", id, "✓ resolved → "+answer), nil)
	}
	return answer, err
}

var (
	kv       = regexp.MustCompile(`([A-Za-z_][\w-]*)=("(?:[^"\\]|\\.)*"|\S*)`)
	slackURL = regexp.MustCompile(`<((?:https?|mailto):[^|>]+)(?:\|[^>]*)?>`)
	entities = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

// unslack undoes Slack's message encoding: <url|label> links and HTML entities.
func unslack(s string) string { return entities.Replace(slackURL.ReplaceAllString(s, "$1")) }

// takesText reports whether a text reply can answer c: an ask (a state's or a
// worker's), a form, or a contract with options when the reply is one of them.
func takesText(c *engine.ContractView, text string) bool {
	if c.Kind == "ask" || c.Kind == "form" || len(c.Fields) > 0 {
		return true
	}
	for _, o := range c.Options {
		if strings.EqualFold(o, strings.TrimSpace(text)) {
			return true
		}
	}
	return false
}

// parseReply turns a thread reply into (answer, data). Forms take
// `answer field=value ...` (values parsed as JSON when they parse); options
// match case-insensitively; anything else is the whole text.
func parseReply(c *engine.ContractView, text string) (string, map[string]any) {
	text = strings.TrimSpace(text)
	var data map[string]any
	if len(c.Fields) > 0 {
		if loc := kv.FindStringIndex(text); loc != nil {
			data = map[string]any{}
			for _, m := range kv.FindAllStringSubmatch(text[loc[0]:], -1) {
				var v any
				if json.Unmarshal([]byte(m[2]), &v) != nil {
					v = m[2]
				}
				data[m[1]] = v
			}
			text = strings.TrimSpace(text[:loc[0]])
		}
	}
	for _, o := range c.Options {
		if strings.EqualFold(o, text) {
			return o, data
		}
	}
	return text, data // kami validates; its refusal goes back to the thread
}

// --- rendering -----------------------------------------------------------------

const maxBody = 2000 // a section block holds 3000 characters

func render(c *engine.ContractView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*Kami decision* · %s / %s (%s)\n*%s*\n", c.Workflow, c.Step, c.Kind, c.Title)
	if body := strings.TrimSpace(c.Body); body != "" {
		if r := []rune(body); len(r) > maxBody {
			body = string(r[:maxBody]) + fmt.Sprintf("\n… (cut at %d of %d characters; `kami contract show %s` has it all)", maxBody, len(r), c.ID)
		}
		b.WriteString(body + "\n")
	}
	b.WriteString("\n")
	if len(c.Fields) > 0 {
		names := make([]string, 0, len(c.Fields))
		for n := range c.Fields {
			names = append(names, n)
		}
		sort.Strings(names)
		var fs []string
		for _, n := range names {
			fs = append(fs, fmt.Sprintf("`%s` (%s)", n, c.Fields[n]))
		}
		if len(c.Options) > 0 {
			fmt.Fprintf(&b, "Answers: `%s`\n", strings.Join(c.Options, "` · `"))
		}
		fmt.Fprintf(&b, "Fields: %s\n", strings.Join(fs, ", "))
	}
	if c.Default != "" {
		fmt.Fprintf(&b, "Default: `%s`\n", c.Default)
	}
	mode := "Mode: " + c.Effective
	if c.Deadline != nil {
		mode += " · deadline " + c.Deadline.Local().Format("Mon 15:04 MST")
	}
	if c.Lock {
		mode += " · 🔒 you only"
	}
	b.WriteString(mode + "\n")
	switch {
	case len(c.Fields) > 0:
		b.WriteString("↩ Reply in this thread: `answer field=value …`")
	case len(c.Options) > 0:
		b.WriteString("Tap an answer, or reply in this thread with one.")
	default:
		b.WriteString("↩ Reply in this thread with your answer (any text).")
	}
	fmt.Fprintf(&b, "\n_contract `%s` · run `%s`_", c.ID, c.RunID)
	return b.String()
}

func section(text string) slack.Block {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)
}

// contractBlocks is the text plus, for a choice-like contract, one button per
// option (action id kami:<contract>:<n>, value = the option).
func contractBlocks(text, id string, options []string, def string, buttons bool) []slack.Block {
	blocks := []slack.Block{section(text)}
	if !buttons {
		return blocks
	}
	var els []slack.BlockElement
	for i, o := range options {
		label := o
		if r := []rune(label); len(r) > 75 {
			label = string(r[:74]) + "…"
		}
		btn := slack.NewButtonBlockElement(actionPrefix+id+":"+strconv.Itoa(i), o,
			slack.NewTextBlockObject(slack.PlainTextType, label, false, false))
		if o == def {
			btn.Style = slack.StylePrimary
		}
		els = append(els, btn)
	}
	return append(blocks, slack.NewActionBlock("kami-"+id, els...))
}
