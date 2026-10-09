package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"kami/internal/engine"
	"kami/internal/eventbus"
)

const owner = "UOWNER"

// fakeKami serves /events, /contracts/{id} and /contracts/{id}/resolve.
type fakeKami struct {
	mu        sync.Mutex
	events    []eventbus.Event
	contracts map[string]*engine.Contract
	resolves  []string // "id=answer data"

	signals []map[string]any
	waiting map[string]bool // thread key -> a chat run waits on slack.message
	busy    map[string]bool // thread key -> an active chat run is working
	started []map[string]any
	runs    map[string]map[string]any // run id -> {"workflow", "parent", "vars"}
}

func (k *fakeKami) handler(t *testing.T) http.Handler {
	env := func(w http.ResponseWriter, v any, code, msg string) {
		if code != "" {
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(engine.Envelope{Error: &engine.APIError{Code: code, Message: msg}})
			return
		}
		raw, _ := json.Marshal(v)
		json.NewEncoder(w).Encode(engine.Envelope{OK: true, Data: raw})
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		for _, ev := range k.events {
			if ev.ID > since {
				json.NewEncoder(w).Encode(ev)
				w.Write([]byte("\n")) // a keepalive line must be skipped
			}
		}
	})
	m.HandleFunc("POST /signal", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		k.signals = append(k.signals, req)
		key, _ := req["key"].(string)
		if req["kekkai"] == "work" && req["type"] == "slack.message" && k.waiting[key] {
			delete(k.waiting, key)
			env(w, map[string]any{"delivered": []string{"run-" + key}}, "", "")
			return
		}
		env(w, map[string]any{"delivered": nil}, "", "")
	})
	m.HandleFunc("GET /runs", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		q := r.URL.Query()
		runs := []map[string]any{}
		key, ok := strings.CutPrefix(q.Get("var"), "thread_ts=")
		if ok && q.Get("status") == "active" && q.Get("kekkai") == "work" && k.busy[key] {
			runs = append(runs, map[string]any{"id": "run-" + key, "workflow": "chat", "status": "running"})
		}
		if ok && q.Get("status") == "" {
			for id, run := range k.runs {
				if vars, _ := run["vars"].(map[string]any); vars["thread_ts"] == key {
					runs = append(runs, map[string]any{"id": id, "workflow": run["workflow"]})
				}
			}
		}
		env(w, runs, "", "")
	})
	m.HandleFunc("GET /runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if run := k.runs[r.PathValue("id")]; run != nil {
			env(w, run, "", "")
			return
		}
		env(w, nil, "NOT_FOUND", "no run "+r.PathValue("id"))
	})
	m.HandleFunc("POST /runs", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["workflow"] != "chat" && req["workflow"] != "mr" {
			env(w, nil, "NOT_FOUND", fmt.Sprintf("no workflow %q in kekkai work", req["workflow"]))
			return
		}
		k.started = append(k.started, req)
		env(w, map[string]any{"id": "run-new"}, "", "")
	})
	m.HandleFunc("GET /contracts/{id}", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		env(w, engine.ContractView{Contract: k.contracts[r.PathValue("id")], Effective: "attended", Workflow: "hello"}, "", "")
	})
	m.HandleFunc("POST /contracts/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		defer k.mu.Unlock()
		if r.Header.Get("X-Kami-Token") != "" {
			t.Error("resolve carried X-Kami-Token")
		}
		var req struct {
			Answer string
			Data   map[string]any
		}
		json.NewDecoder(r.Body).Decode(&req)
		c := k.contracts[r.PathValue("id")]
		k.resolves = append(k.resolves, fmt.Sprintf("%s=%s %v", c.ID, req.Answer, req.Data))
		if len(c.Options) > 0 && !contains(c.Options, req.Answer) {
			env(w, nil, "INVALID", fmt.Sprintf("answer %q is not one of: %s", req.Answer, strings.Join(c.Options, ", ")))
			return
		}
		if c.Fields != nil && req.Data["rating"] != "good" && req.Data["rating"] != "bad" {
			env(w, nil, "INVALID", fmt.Sprintf("field \"rating\": want enum:good,bad, got %v", req.Data["rating"]))
			return
		}
		c.Status, c.Resolution = "resolved", &engine.Resolution{Answer: req.Answer, By: "human"}
		env(w, engine.ContractView{Contract: c}, "", "")
	})
	return m
}

type call struct {
	Method string
	Form   url.Values
}

// fakeSlack answers the Web API methods kami-slack uses and records each call.
type fakeSlack struct {
	mu      sync.Mutex
	calls   []call
	n       int
	replies map[string][]map[string]any // thread ts -> conversations.replies messages
}

func (s *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ParseForm()
	method := strings.TrimPrefix(r.URL.Path, "/")
	s.calls = append(s.calls, call{method, r.Form})
	var out map[string]any
	switch method {
	case "conversations.open":
		out = map[string]any{"channel": map[string]any{"id": "D1"}}
	case "chat.postMessage":
		if r.Form.Get("thread_ts") == "999.9" {
			out = map[string]any{"ok": false, "error": "invalid_thread_ts"}
			break
		}
		s.n++
		out = map[string]any{"channel": "D1", "ts": fmt.Sprintf("100.%d", s.n)}
	case "chat.update":
		out = map[string]any{"channel": "D1", "ts": r.Form.Get("ts")}
	case "canvases.create":
		s.n++
		out = map[string]any{"canvas_id": fmt.Sprintf("F%d", s.n)}
	case "conversations.replies":
		// two per page, so every caller has to follow the cursor
		msgs, from := s.replies[r.Form.Get("ts")], 0
		if c := r.Form.Get("cursor"); c != "" {
			from, _ = strconv.Atoi(c)
		}
		to := min(from+2, len(msgs))
		out = map[string]any{"messages": msgs[from:to], "has_more": to < len(msgs)}
		if to < len(msgs) {
			out["response_metadata"] = map[string]any{"next_cursor": strconv.Itoa(to)}
		}
	case "files.getUploadURLExternal":
		out = map[string]any{"upload_url": "http://" + r.Host + "/upload", "file_id": "FPIC"}
	case "upload":
		w.Write([]byte("OK"))
		return
	case "files.completeUploadExternal":
		out = map[string]any{"files": []map[string]any{{"id": "FPIC", "title": "power.gif"}}}
	case "canvases.edit", "canvases.access.set", "reactions.add", "pins.add", "pins.remove":
		out = map[string]any{}
	case "files.info":
		out = map[string]any{"file": map[string]any{"id": r.Form.Get("file"), "permalink": "https://x.slack.com/docs/T1/" + r.Form.Get("file")}}
	default:
		out = map[string]any{"ok": false, "error": "unknown_method"}
	}
	if _, set := out["ok"]; !set {
		out["ok"] = true
	}
	json.NewEncoder(w).Encode(out)
}

// of returns the recorded calls of one method.
func (s *fakeSlack) of(method string) []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []url.Values
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c.Form)
		}
	}
	return out
}

func setup(t *testing.T) (*fakeKami, *fakeSlack, func() *adapter) {
	dir, err := os.MkdirTemp("", "ks") // short: unix socket paths cap at 103 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	k := &fakeKami{contracts: map[string]*engine.Contract{}, waiting: map[string]bool{}, busy: map[string]bool{}, runs: map[string]map[string]any{}}
	sock := filepath.Join(dir, "k.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ks := httptest.NewUnstartedServer(k.handler(t))
	ks.Listener.Close()
	ks.Listener = l
	ks.Start()
	t.Cleanup(ks.Close)
	s := &fakeSlack{replies: map[string][]map[string]any{}}
	ss := httptest.NewServer(s)
	t.Cleanup(ss.Close)
	start := func() *adapter {
		a := &adapter{api: slack.New("xoxb-fake", slack.OptionAPIURL(ss.URL+"/")), kami: kamiClient(sock),
			owner: owner, dir: filepath.Join(dir, "adapters"), chatKekkai: "work", chatWorkflow: "chat", replyAnswers: true}
		if err := a.load(); err != nil {
			t.Fatal(err)
		}
		return a
	}
	return k, s, start
}

func tap(user, contract, option string) slack.InteractionCallback {
	return slack.InteractionCallback{Type: slack.InteractionTypeBlockActions, User: slack.User{ID: user},
		Channel:        slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "D1"}}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{ActionID: "kami:" + contract + ":0", Value: option}}}}
}

func reply(user, thread, ts, text string) *slackevents.MessageEvent {
	return &slackevents.MessageEvent{ChannelType: "im", Channel: "D1", User: user, ThreadTimeStamp: thread, TimeStamp: ts, Text: text}
}

func TestServe(t *testing.T) {
	ctx := context.Background()
	k, s, start := setup(t)
	k.contracts["c1"] = &engine.Contract{ID: "c1", RunID: "r1", Step: "review", Kind: "choice", Title: "Keep the draft?",
		Body: "three lines about tea", Options: []string{"keep", "redo"}, Default: "keep", Status: "open", Lock: true}
	k.contracts["c2"] = &engine.Contract{ID: "c2", RunID: "r1", Step: "rate", Kind: "form", Title: "Rate it",
		Fields: map[string]string{"rating": "enum:good,bad"}, Options: []string{"rated"}, Status: "open"}
	k.contracts["c3"] = &engine.Contract{ID: "c3", RunID: "r2", Kind: "ask", Title: "Which port?", Status: "open"}
	for i, id := range []string{"c1", "c2", "c3"} {
		k.events = append(k.events, eventbus.Event{ID: int64(i + 1), Type: "contract.opened", Data: map[string]any{"id": id}})
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// opened → one owner DM each; buttons only for the choice
	a := start()
	must(a.events(ctx))
	posts := s.of("chat.postMessage")
	if len(posts) != 3 || len(s.of("conversations.open")) != 1 || s.of("conversations.open")[0].Get("users") != owner {
		t.Fatalf("posts = %v", posts)
	}
	if b := posts[0].Get("blocks"); !strings.Contains(b, `"action_id":"kami:c1:0"`) || !strings.Contains(b, `"value":"redo"`) ||
		!strings.Contains(posts[0].Get("text"), "Keep the draft?") || !strings.Contains(posts[0].Get("text"), "you only") {
		t.Fatalf("c1 post = %v", posts[0])
	}
	if b := posts[1].Get("blocks"); strings.Contains(b, "action_id") || !strings.Contains(posts[1].Get("text"), "`rating` (enum:good,bad)") {
		t.Fatalf("c2 post = %v", posts[1])
	}
	if th := a.st.Threads; th["c1"].TS != "100.1" || !th["c1"].Buttons || th["c2"].TS != "100.2" || th["c2"].Buttons || a.st.Since != 3 {
		t.Fatalf("state = %+v", a.st)
	}

	// restart from the state file: nothing re-posted
	a = start()
	must(a.events(ctx))
	if n := len(s.of("chat.postMessage")); n != 3 {
		t.Fatalf("restart re-posted: %d posts", n)
	}

	// someone else's tap and thread reply are ignored
	must(a.onAction(ctx, tap("UOTHER", "c1", "keep")))
	must(a.onMessage(ctx, reply("UOTHER", "100.2", "200.1", "rated rating=good")))
	if len(k.resolves) != 0 || len(s.of("chat.postMessage")) != 3 {
		t.Fatalf("non-owner acted: %v", k.resolves)
	}

	// owner taps keep → resolved, message redrawn without buttons, no thread post
	must(a.onAction(ctx, tap(owner, "c1", "keep")))
	up := s.of("chat.update")
	if len(k.resolves) != 1 || k.resolves[0] != "c1=keep map[]" || len(up) != 1 || up[0].Get("ts") != "100.1" ||
		strings.Contains(up[0].Get("blocks"), "action_id") || !strings.Contains(up[0].Get("blocks"), "answered *keep* by \\u003c@UOWNER\\u003e") {
		t.Fatalf("tap: resolves %v update %v", k.resolves, up)
	}

	// invalid form answer → kami's refusal in the thread; then a valid one
	must(a.onMessage(ctx, reply(owner, "100.2", "200.2", "rated rating=meh")))
	last := func() url.Values { p := s.of("chat.postMessage"); return p[len(p)-1] }
	if l := last(); l.Get("thread_ts") != "100.2" || !strings.Contains(l.Get("text"), `field "rating": want enum:good,bad, got meh`) {
		t.Fatalf("refusal = %v", l)
	}
	must(a.onMessage(ctx, reply(owner, "100.2", "200.3", "Rated rating=good")))
	if k.resolves[2] != "c2=rated map[rating:good]" || last().Get("text") != "✓ Got it: rated. POWER approves." {
		t.Fatalf("form: resolves %v last %v", k.resolves, last())
	}

	if len(k.signals) != 0 || len(k.started) != 0 || len(s.of("reactions.add")) != 0 {
		t.Fatalf("contract replies routed to chat: signals %v runs %v", k.signals, k.started)
	}

	// our own resolutions get no follow-up; one by the AFK resolver does
	k.mu.Lock()
	k.events = append(k.events,
		eventbus.Event{ID: 4, Type: "contract.resolved", Data: map[string]any{"id": "c1", "answer": "keep", "by": "human"}},
		eventbus.Event{ID: 5, Type: "contract.resolved", Data: map[string]any{"id": "c2", "answer": "rated", "by": "human"}},
		eventbus.Event{ID: 6, Type: "contract.resolved", Data: map[string]any{"id": "c3", "answer": "8080", "by": "resolver"}})
	k.mu.Unlock()
	before := len(s.of("chat.postMessage"))
	must(a.events(ctx))
	if p := s.of("chat.postMessage"); len(p) != before+1 || p[before].Get("thread_ts") != "100.3" ||
		!strings.Contains(p[before].Get("text"), "resolver already answered: 8080") || len(a.st.Threads) != 0 {
		t.Fatalf("follow-ups = %v threads %v", p[before:], a.st.Threads)
	}
}

func TestCanvasAndDM(t *testing.T) {
	ctx := context.Background()
	_, s, start := setup(t)
	a := start()
	u1, err := a.canvas(ctx, "weekly", "Weekly", "# one", nil)
	if err != nil {
		t.Fatal(err)
	}
	u2, err := a.canvas(ctx, "weekly", "Weekly", "# two", nil)
	if err != nil {
		t.Fatal(err)
	}
	if u1 != u2 || len(s.of("canvases.create")) != 1 || len(s.of("canvases.edit")) != 1 {
		t.Fatalf("keyed canvas: %s %s, %d creates", u1, u2, len(s.of("canvases.create")))
	}
	acc := s.of("canvases.access.set")
	if len(acc) != 1 || acc[0].Get("access_level") != "write" || acc[0].Get("user_ids") != `["UOWNER"]` || acc[0].Get("channel_ids") != "" {
		t.Fatalf("access = %v", acc)
	}
	if !strings.Contains(s.of("canvases.edit")[0].Get("changes"), "# two") {
		t.Fatalf("edit = %v", s.of("canvases.edit"))
	}
	if _, ts, err := a.post(ctx, entry{Kind: "thread_reply", ThreadTS: "100.9", Text: "in thread"}, nil); err != nil || s.of("chat.postMessage")[0].Get("thread_ts") != "100.9" || ts == "" {
		t.Fatalf("dm --thread: %v %v", err, s.of("chat.postMessage"))
	}
	if _, err := a.canvas(ctx, "../x", "t", "", nil); err == nil {
		t.Fatal("a path-like key was accepted")
	}
	ch, ts, err := a.post(ctx, entry{Kind: "dm", Text: "hello"}, nil)
	if err != nil || ch != "D1" || ts == "" {
		t.Fatalf("dm = %s %s %v", ch, ts, err)
	}
}

func TestParseReply(t *testing.T) {
	form := &engine.ContractView{Contract: &engine.Contract{Options: []string{"rated"},
		Fields: map[string]string{"rating": "enum:good,bad", "replay": "bool", "note": "string?"}}}
	ans, data := parseReply(form, `Rated rating=good replay=true note="two words"`)
	if ans != "rated" || data["rating"] != "good" || data["replay"] != true || data["note"] != "two words" {
		t.Fatalf("form = %q %v", ans, data)
	}
	ask := &engine.ContractView{Contract: &engine.Contract{Kind: "ask"}}
	if ans, _ := parseReply(ask, unslack("  see <https://x.io/a?b=1&amp;c=2|x.io> port=8080 ")); ans != "see https://x.io/a?b=1&c=2 port=8080" {
		t.Fatalf("ask = %q", ans)
	}
}

func TestChat(t *testing.T) {
	ctx := context.Background()
	k, s, start := setup(t)
	a := start()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	inputs := func(i int) map[string]any { return k.started[i]["inputs"].(map[string]any) }

	// a non-owner DM is ignored
	must(a.onMessage(ctx, reply("UOTHER", "", "290.1", "hi")))
	if len(k.signals)+len(k.started)+len(s.of("reactions.add")) != 0 {
		t.Fatal("non-owner DM routed")
	}

	// top-level DM: 👀, signal (nobody waiting), no run owns it → a run starts under the message itself
	must(a.onMessage(ctx, reply(owner, "", "300.1", "deploy &lt;staging&gt;")))
	if r := s.of("reactions.add"); len(r) != 1 || r[0].Get("name") != "eyes" || r[0].Get("timestamp") != "300.1" || r[0].Get("channel") != "D1" {
		t.Fatalf("reaction = %v", r)
	}
	if len(k.signals) != 1 || k.signals[0]["key"] != "300.1" || k.signals[0]["data"].(map[string]any)["text"] != "deploy <staging>" {
		t.Fatalf("signals = %v", k.signals)
	}
	if len(k.started) != 1 || k.started[0]["kekkai"] != "work" || k.started[0]["workflow"] != "chat" ||
		inputs(0)["thread_ts"] != "300.1" || inputs(0)["channel"] != "D1" || inputs(0)["text"] != "deploy <staging>" || inputs(0)["history"] != "" {
		t.Fatalf("started = %v", k.started)
	}
	if len(s.of("conversations.replies")) != 0 || len(a.st.Queue) != 0 {
		t.Fatalf("top-level DM fetched history or left a queue: %v", a.st.Queue)
	}

	// reply while the run waits for it → signal delivered, no new run
	k.waiting["300.1"] = true
	must(a.onMessage(ctx, reply(owner, "300.1", "300.2", "and prod")))
	if len(k.started) != 1 || len(a.st.Queue) != 0 || k.signals[len(k.signals)-1]["data"].(map[string]any)["ts"] != "300.2" {
		t.Fatalf("waiting run: started %d queue %v", len(k.started), a.st.Queue)
	}

	// reply while the run is busy → queued (in order), then delivered one per tick once it waits
	k.busy["300.1"] = true
	must(a.onMessage(ctx, reply(owner, "300.1", "300.3", "one")))
	must(a.onMessage(ctx, reply(owner, "300.1", "300.4", "two")))
	if q := a.st.Queue["300.1"]; len(q) != 2 || q[0].Text != "one" || q[1].Text != "two" || len(k.started) != 1 {
		t.Fatalf("busy: queue %v started %d", q, len(k.started))
	}
	a = start() // the queue survives a restart
	k.waiting["300.1"] = true
	a.routeAll(ctx)
	if q := a.st.Queue["300.1"]; len(q) != 1 || q[0].Text != "two" || k.signals[len(k.signals)-1]["data"].(map[string]any)["text"] != "one" {
		t.Fatalf("after tick 1: queue %v", q)
	}
	k.waiting["300.1"] = true
	a.routeAll(ctx)
	if len(a.st.Queue) != 0 || k.signals[len(k.signals)-1]["data"].(map[string]any)["text"] != "two" || len(k.started) != 1 {
		t.Fatalf("after tick 2: queue %v started %d", a.st.Queue, len(k.started))
	}

	// reply in an older thread no run owns → a run starts with the thread so far, current message excluded
	s.replies["400.1"] = []map[string]any{
		{"ts": "400.1", "user": owner, "text": "first"},
		{"ts": "400.2", "user": "UBOT", "bot_id": "B1", "text": "answer"},
		{"ts": "400.5", "user": owner, "text": "now"},
	}
	must(a.onMessage(ctx, reply(owner, "400.1", "400.5", "now")))
	if len(k.started) != 2 || inputs(1)["thread_ts"] != "400.1" || inputs(1)["text"] != "now" || inputs(1)["history"] != "owner: first\nbot: answer" {
		t.Fatalf("thread run = %v", k.started[len(k.started)-1])
	}

	// a run that cannot start → kami's error in the thread
	a.chatWorkflow = "nope"
	must(a.onMessage(ctx, reply(owner, "", "500.1", "hello")))
	p := s.of("chat.postMessage")
	if l := p[len(p)-1]; l.Get("thread_ts") != "500.1" || !strings.Contains(l.Get("text"), `no workflow "nope"`) || len(a.st.Queue) != 0 {
		t.Fatalf("start failure post = %v queue %v", l, a.st.Queue)
	}
}

func TestTranscript(t *testing.T) {
	ctx := context.Background()
	k, _, start := setup(t)
	k.contracts["c1"] = &engine.Contract{ID: "c1", RunID: "r1", Kind: "choice", Title: "Keep?", Options: []string{"keep", "redo"}, Status: "open"}
	k.contracts["c2"] = &engine.Contract{ID: "c2", RunID: "r2", Kind: "ask", Title: "Port?", Status: "open"}
	k.events = []eventbus.Event{{ID: 1, Type: "contract.opened", Data: map[string]any{"id": "c1"}},
		{ID: 2, Type: "contract.opened", Data: map[string]any{"id": "c2"}}}
	a := start()
	a.run = "rstep"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(a.events(ctx))                                            // contract out ×2 (100.1, 100.2)
	must(a.onAction(ctx, tap(owner, "c1", "keep")))                // button in
	must(a.onMessage(ctx, reply(owner, "100.2", "200.1", "8080"))) // thread_reply in + followup out
	must(a.onMessage(ctx, reply(owner, "", "300.1", "hi")))        // dm in, reaction out, chat_routed started
	k.busy["300.1"] = true
	must(a.onMessage(ctx, reply(owner, "300.1", "300.2", "more"))) // chat_routed queued
	k.waiting["300.1"] = true
	a.routeAll(ctx) // chat_routed signalled
	must(a.onMessage(ctx, reply("UOTHER", "", "301.1", "psst")))
	if _, _, err := a.post(ctx, entry{Kind: "dm", Text: "line one\nline two"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.canvas(ctx, "", "Weekly", "# body", nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(a.transcriptPath())
	must(err)
	if strings.Contains(string(raw), "xoxb-fake") || strings.Contains(strings.ToLower(string(raw)), "authorization") {
		t.Fatalf("token or header in transcript:\n%s", raw)
	}
	if fi, _ := os.Stat(a.transcriptPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	es, err := readTranscript(a.transcriptPath())
	must(err)
	var got []string
	for _, e := range es {
		if _, err := time.Parse(time.RFC3339Nano, e.LoggedAt); err != nil {
			t.Fatalf("logged_at %q", e.LoggedAt)
		}
		got = append(got, e.Dir+":"+e.Kind)
	}
	want := "out:contract out:contract in:button in:thread_reply out:followup in:dm out:reaction in:chat_routed " +
		"in:thread_reply out:reaction in:chat_routed in:chat_routed in:dm out:dm out:canvas"
	if strings.Join(got, " ") != want {
		t.Fatalf("kinds:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	find := func(kind string, n int) entry {
		for _, e := range es {
			if e.Kind == kind {
				if n == 0 {
					return e
				}
				n--
			}
		}
		t.Fatalf("no %s #%d", kind, n)
		return entry{}
	}
	if e := find("contract", 0); e.ContractID != "c1" || e.RunID != "r1" || e.Workflow != "hello" || e.TS != "100.1" || e.Channel != "D1" || !strings.Contains(e.Text, "Keep?") {
		t.Fatalf("contract = %+v", e)
	}
	if e := find("button", 0); e.Value != "keep" || e.Answer != "keep" || e.ContractID != "c1" || e.RunID != "r1" || e.User != owner || e.TS != "100.1" {
		t.Fatalf("button = %+v", e)
	}
	if e := find("thread_reply", 0); e.Dir != "in" || e.ContractID != "c2" || e.RunID != "r2" || e.Text != "8080" || e.ThreadTS != "100.2" || e.User != owner {
		t.Fatalf("thread reply = %+v", e)
	}
	if e := find("followup", 0); e.ThreadTS != "100.2" || e.ContractID != "c2" || e.Text != "✓ Got it: 8080. POWER approves." || e.User != "" {
		t.Fatalf("followup = %+v", e)
	}
	started, queued, signalled := find("chat_routed", 0), find("chat_routed", 1), find("chat_routed", 2)
	if started.Routed != "started" || started.RunID != "run-new" || started.ThreadTS != "300.1" ||
		queued.Routed != "queued" || queued.TS != "300.2" ||
		signalled.Routed != "signalled" || signalled.TS != "300.2" || strings.Join(signalled.Delivered, ",") != "run-300.1" {
		t.Fatalf("chat routing = %+v / %+v / %+v", started, queued, signalled)
	}
	if e := find("dm", 1); e.User != "UOTHER" || e.Error == "" {
		t.Fatalf("non-owner dm = %+v", e)
	}
	if e := find("dm", 2); e.Text != "line one\nline two" || e.RunID != "rstep" {
		t.Fatalf("dm out = %+v", e)
	}
	if e := find("canvas", 0); e.Text != "# body" || e.Title != "Weekly" || !strings.HasPrefix(e.CanvasURL, "https://") {
		t.Fatalf("canvas = %+v", e)
	}

	// a failed post is an error entry
	a.im = ""
	a.api = slack.New("xoxb-fake", slack.OptionAPIURL("http://127.0.0.1:1/"))
	if _, _, err := a.post(ctx, entry{Kind: "dm", Text: "lost"}, nil); err == nil {
		t.Fatal("post to a dead Slack succeeded")
	}
	es, _ = readTranscript(a.transcriptPath())
	if e := es[len(es)-1]; e.Kind != "error" || e.Text != "lost" || e.Error == "" {
		t.Fatalf("error entry = %+v", e)
	}
}

func writeTranscript(t *testing.T, a *adapter, es ...entry) {
	t.Helper()
	for _, e := range es {
		a.record(e)
	}
}

func TestLogFilters(t *testing.T) {
	_, _, start := setup(t)
	a := start()
	writeTranscript(t, a,
		entry{Dir: "out", Kind: "contract", TS: "1.1", Text: "q", ContractID: "c1", RunID: "r1"},
		entry{Dir: "in", Kind: "thread_reply", TS: "1.2", ThreadTS: "1.1", Text: "yes", ContractID: "c1", RunID: "r1"},
		entry{Dir: "in", Kind: "chat_routed", TS: "2.2", ThreadTS: "2.1", Text: "hi", Routed: "signalled", Delivered: []string{"r9"}},
		entry{Dir: "out", Kind: "dm", TS: "3.1", Text: "other"})
	run := func(f filter, asJSON bool) string {
		var b strings.Builder
		if err := printLog(context.Background(), &b, a.transcriptPath(), f, false, asJSON); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if out := run(filter{run: "r1"}, true); strings.Count(out, "\n") != 2 || strings.Contains(out, "other") {
		t.Fatalf("--run r1:\n%s", out)
	}
	if out := run(filter{run: "r9"}, false); !strings.Contains(out, "routed=signalled delivered=r9") || !strings.Contains(out, "\n    hi") {
		t.Fatalf("--run r9 (delivered):\n%s", out)
	}
	if out := run(filter{thread: "1.1"}, true); strings.Count(out, "\n") != 2 {
		t.Fatalf("--thread 1.1:\n%s", out)
	}
	if out := run(filter{contract: "c1", thread: "2.1"}, true); out != "" {
		t.Fatalf("contract and thread:\n%s", out)
	}
	if out := run(filter{}, true); strings.Count(out, "\n") != 4 {
		t.Fatalf("all:\n%s", out)
	}

	// -f picks up a line written after it started
	var mu sync.Mutex
	var b strings.Builder
	w := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return b.Write(p) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- printLog(ctx, w, a.transcriptPath(), filter{thread: "4.1"}, true, true) }()
	time.Sleep(100 * time.Millisecond)
	a.record(entry{Dir: "out", Kind: "dm", TS: "4.1", Text: "late"})
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := b.String()
		mu.Unlock()
		if strings.Contains(got, "late") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("follow missed the new line: %q", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestThreads(t *testing.T) {
	now := time.Now()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339Nano) }
	es := []entry{
		{LoggedAt: at(3 * time.Hour), Dir: "in", Kind: "dm", TS: "10.1", Text: "deploy staging\nplease"},
		{LoggedAt: at(3 * time.Hour), Dir: "out", Kind: "reaction", TS: "10.1", Text: "eyes"},
		{LoggedAt: at(3 * time.Hour), Dir: "in", Kind: "chat_routed", TS: "10.1", ThreadTS: "10.1", Routed: "started", RunID: "rA"},
		{LoggedAt: at(2 * time.Hour), Dir: "out", Kind: "thread_reply", TS: "10.2", ThreadTS: "10.1", Text: "done", RunID: "rA"},
		{LoggedAt: at(1 * time.Hour), Dir: "in", Kind: "chat_routed", TS: "10.3", ThreadTS: "10.1", Routed: "signalled", Delivered: []string{"rA"}},
		{LoggedAt: at(1 * time.Hour), Dir: "in", Kind: "thread_reply", TS: "10.3", ThreadTS: "10.1", Text: "thanks"},
		{LoggedAt: at(30 * time.Minute), Dir: "out", Kind: "contract", TS: "20.1", Text: "*Kami decision*\nKeep?", ContractID: "c1", RunID: "rB"},
		{LoggedAt: at(20 * time.Minute), Dir: "in", Kind: "button", TS: "20.1", ContractID: "c1", RunID: "rB", Value: "keep"},
		{LoggedAt: at(48 * time.Hour), Dir: "out", Kind: "dm", TS: "5.1", Text: "old"},
		{LoggedAt: at(time.Minute), Dir: "out", Kind: "canvas", Text: "# x"},
	}
	sums := summarize(es, now.Add(-24*time.Hour))
	if len(sums) != 2 {
		t.Fatalf("sums = %+v", sums)
	}
	c, d := sums[0], sums[1]
	if c.Root != "20.1" || c.Messages != 1 || c.FirstLine != "*Kami decision*" || strings.Join(c.Contracts, ",") != "c1" || strings.Join(c.Runs, ",") != "rB" {
		t.Fatalf("contract thread = %+v", c)
	}
	if d.Root != "10.1" || d.Messages != 3 || d.FirstLine != "deploy staging" || strings.Join(d.Runs, ",") != "rA" || len(d.Contracts) != 0 ||
		d.Last.Format(time.RFC3339Nano) != at(time.Hour) {
		t.Fatalf("chat thread = %+v", d)
	}
	if all := summarize(es, time.Time{}); len(all) != 3 {
		t.Fatalf("no --since: %d threads", len(all))
	}
	var b strings.Builder
	if err := printThreads(&b, sums, false); err != nil || !strings.Contains(b.String(), "10.1") || !strings.Contains(b.String(), "deploy staging") {
		t.Fatalf("table = %q %v", b.String(), err)
	}
	b.Reset()
	if err := printThreads(&b, nil, true); err != nil || b.String() != "[]\n" {
		t.Fatalf("empty json = %q", b.String())
	}
}

func TestThread(t *testing.T) {
	ctx := context.Background()
	_, s, start := setup(t)
	a := start()
	s.replies["1712345678.000100"] = []map[string]any{
		{"ts": "1712345678.000100", "user": owner, "text": "deploy"},
		{"ts": "1712345690.000200", "user": "UBOT", "bot_id": "B1", "text": "on it\ntwo lines"},
		{"ts": "1712345699.000300", "user": owner, "text": "thanks"},
	}
	writeTranscript(t, a,
		entry{Dir: "in", Kind: "chat_routed", TS: "1712345678.000100", ThreadTS: "1712345678.000100", Routed: "started", RunID: "rX"},
		entry{Dir: "out", Kind: "thread_reply", TS: "1712345690.000200", ThreadTS: "1712345678.000100", RunID: "rX", ContractID: "cY"})
	var b strings.Builder
	if err := a.printThread(ctx, &b, "1712345678.000100", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(b.String(), "\n")
	want0 := time.Unix(1712345678, 100000).Local().Format("2006-01-02 15:04:05") + " owner 1712345678.000100 run=rX"
	if len(lines) < 5 || lines[0] != want0 || lines[1] != "    deploy" ||
		!strings.HasSuffix(lines[2], "bot   1712345690.000200 contract=cY run=rX") || lines[3] != "    on it" || lines[4] != "    two lines" {
		t.Fatalf("thread:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "owner 1712345699.000300\n    thanks") {
		t.Fatalf("second page missing:\n%s", b.String())
	}
	if r := s.of("conversations.replies"); len(r) != 2 || r[0].Get("channel") != "D1" || r[0].Get("ts") != "1712345678.000100" || r[1].Get("cursor") != "2" {
		t.Fatalf("replies call = %v", r)
	}
	b.Reset()
	if err := a.printThread(ctx, &b, "1712345678.000100", true); err != nil {
		t.Fatal(err)
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(b.String()), &raw); err != nil || len(raw) != 3 || raw[1]["bot_id"] != "B1" {
		t.Fatalf("json = %s %v", b.String(), err)
	}
}

func TestRunThreads(t *testing.T) {
	ctx := context.Background()
	k, s, start := setup(t)
	now := time.Now()
	k.runs["rp"] = map[string]any{"id": "rp", "workflow": "build", "vars": map[string]any{"thread_ts": "700.1"}}
	k.runs["rc"] = map[string]any{"id": "rc", "workflow": "lint", "parent": "rp", "vars": map[string]any{}}
	k.runs["rn"] = map[string]any{"id": "rn", "workflow": "solo", "vars": map[string]any{}}
	k.runs["rq"] = map[string]any{"id": "rq", "workflow": "release", "vars": map[string]any{"thread_ts": "800.1"}}
	k.contracts["c1"] = &engine.Contract{ID: "c1", RunID: "rc", Origin: "state", Kind: "ask", Title: "Which port?", Status: "open"}
	k.contracts["cN"] = &engine.Contract{ID: "cN", RunID: "rn", Origin: "state", Kind: "choice", Title: "Keep?", Options: []string{"keep", "redo"}, Status: "open"}
	k.contracts["c5"] = &engine.Contract{ID: "c5", RunID: "rq", Origin: "engine", Kind: "blocker", Title: "Ship?", Options: []string{"ship", "hold"}, Status: "open"}
	ev := func(id int64, typ, run string, data map[string]any) eventbus.Event {
		return eventbus.Event{ID: id, TS: now, Type: typ, Run: run, Data: data}
	}
	k.events = []eventbus.Event{
		ev(1, "contract.opened", "rc", map[string]any{"id": "c1"}),
		ev(2, "contract.opened", "rn", map[string]any{"id": "cN"}),
		ev(3, "run.started", "rc", map[string]any{"workflow": "lint", "parent": "rp"}),
		ev(4, "run.started", "rp", map[string]any{"workflow": "build", "parent": ""}),
		ev(5, "run.say", "rc", map[string]any{"state": "lint", "text": "lint: 3 files"}),
		ev(6, "run.say", "rn", map[string]any{"state": "x", "text": "nobody sees this"}),
		ev(7, "run.finished", "rc", map[string]any{"status": "done", "outcome": "ok", "report": "clean"}),
		ev(8, "run.finished", "rn", map[string]any{"status": "failed", "report": "boom"}),
		ev(9, "step.started", "rc", map[string]any{"state": "lint"}),
		{ID: 10, TS: now.Add(-time.Hour), Type: "run.say", Run: "rc", Data: map[string]any{"text": "stale"}},
		ev(11, "run.finished", "rp", map[string]any{"status": "failed", "report": "lint broke"}),
		ev(12, "contract.opened", "rq", map[string]any{"id": "c5"}),
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	a := start()
	must(a.events(ctx))
	type post struct{ thread, text string }
	var got []post
	for _, p := range s.of("chat.postMessage") {
		got = append(got, post{p.Get("thread_ts"), strings.SplitN(p.Get("text"), "\n", 3)[min(1, strings.Count(p.Get("text"), "\n"))]})
	}
	// rp's milestones and its child's go on one pinned card; only rp's result posts
	want := []post{{"700.1", "*Which port?*"}, {"", "*Keep?*"}, {"700.1", "• ↳ lint `rc`: started"},
		{"700.1", "❌ build failed: lint broke. Not POWER's fault!"}, {"800.1", "*Ship?*"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("posts:\n got %v\nwant %v", got, want)
	}
	posts := s.of("chat.postMessage")
	if !strings.Contains(posts[4].Get("blocks"), `"action_id":"kami:c5:0"`) || a.st.Since != 12 {
		t.Fatalf("c5 in its thread lost its buttons: %v (since %d)", posts[4], a.st.Since)
	}
	ups := s.of("chat.update")
	if card := ups[len(ups)-1]; card.Get("ts") != posts[2].Get("ts") && card.Get("ts") != "100.3" ||
		!strings.Contains(card.Get("text"), "• ↳ lint `rc`: ✅ lint: clean. POWER wins again!") || !strings.Contains(card.Get("text"), "*❌ failed*") ||
		strings.Count(card.Get("text"), "↳") != 1 {
		t.Fatalf("card = %v", card)
	}
	if p := s.of("pins.add"); len(p) != 1 || p[0].Get("timestamp") != "100.3" {
		t.Fatalf("pins = %v", p)
	}
	if th := a.st.Threads; th["c1"].Thread != "700.1" || th["c1"].TS != "100.1" || th["cN"].Thread != "" || th["c5"].Thread != "800.1" {
		t.Fatalf("threads = %+v %+v %+v", th["c1"], th["cN"], th["c5"])
	}
	es, _ := readTranscript(a.transcriptPath())
	var ms []string
	for _, e := range es {
		if e.Kind == "milestone" {
			ms = append(ms, e.RunID+"/"+e.Workflow+"@"+e.ThreadTS)
		}
		if e.Kind == "contract" && e.ContractID == "c1" && e.ThreadTS != "700.1" {
			t.Fatalf("contract entry = %+v", e)
		}
	}
	if strings.Join(ms, " ") != "rp/build@700.1" {
		t.Fatalf("milestone entries = %v", ms)
	}

	// the top-level run that finished in the thread is told to the chat listening there
	if len(k.signals) != 1 || k.signals[0]["key"] != "700.1" ||
		k.signals[0]["data"].(map[string]any)["text"] != "[kami] build failed: lint broke. Not POWER's fault! (run rp)" {
		t.Fatalf("finish signals = %v", k.signals)
	}
	k.signals = nil

	last := func() url.Values { p := s.of("chat.postMessage"); return p[len(p)-1] }
	// a reply in the run thread answers its one open ask
	must(a.onMessage(ctx, reply(owner, "700.1", "701.1", "use port 8080")))
	if len(k.resolves) != 1 || k.resolves[0] != "c1=use port 8080 map[]" || last().Get("thread_ts") != "700.1" || last().Get("text") != "✓ Got it: use port 8080. POWER approves." {
		t.Fatalf("ask in run thread: resolves %v last %v", k.resolves, last())
	}
	if len(k.signals) != 0 {
		t.Fatalf("answered reply also went to chat: %v", k.signals)
	}
	es, _ = readTranscript(a.transcriptPath())
	if e := es[len(es)-2]; e.Kind != "thread_reply" || e.ContractID != "c1" || e.RunID != "rc" {
		t.Fatalf("reply entry = %+v", e)
	}

	// old-style: a reply under a top-level contract message still answers it
	must(a.onMessage(ctx, reply(owner, "100.2", "702.1", "Redo")))
	if k.resolves[1] != "cN=redo map[]" {
		t.Fatalf("old-style: %v", k.resolves)
	}

	// a reply that is none of the blocker's options goes to chat, with the thread's runs
	must(a.onMessage(ctx, reply(owner, "800.1", "801.1", "ship it tomorrow")))
	if len(k.resolves) != 2 || len(k.started) != 1 || inputsOf(k, 0)["thread_ts"] != "800.1" || inputsOf(k, 0)["thread_runs"] != "rq" {
		t.Fatalf("no taker: resolves %v started %v", k.resolves, k.started)
	}
	// one that is an option answers it (and a button tap in a run thread works as before)
	must(a.onMessage(ctx, reply(owner, "800.1", "801.2", "Ship")))
	if len(k.resolves) != 3 || k.resolves[2] != "c5=ship map[]" || last().Get("thread_ts") != "800.1" {
		t.Fatalf("option reply: %v last %v", k.resolves, last())
	}

	// two text-taking contracts in one thread: ambiguous → chat
	k.contracts["c2"] = &engine.Contract{ID: "c2", RunID: "rc", Origin: "state", Kind: "ask", Title: "Name?", Status: "open"}
	k.contracts["c3"] = &engine.Contract{ID: "c3", RunID: "rp", Origin: "state", Kind: "form", Title: "Rate", Fields: map[string]string{"rating": "enum:good,bad"}, Status: "open"}
	k.mu.Lock()
	k.events = append(k.events, ev(13, "contract.opened", "rc", map[string]any{"id": "c2"}), ev(14, "contract.opened", "rp", map[string]any{"id": "c3"}))
	k.mu.Unlock()
	must(a.events(ctx))
	must(a.onMessage(ctx, reply(owner, "700.1", "703.1", "hello")))
	if len(k.resolves) != 3 || len(k.started) != 2 || inputsOf(k, 1)["thread_ts"] != "700.1" || inputsOf(k, 1)["thread_runs"] != "rc,rp" {
		t.Fatalf("ambiguous: resolves %v started %v", k.resolves, k.started)
	}

	// follow-ups land in the run thread, not under the contract message
	k.mu.Lock()
	k.events = append(k.events, ev(15, "contract.cancelled", "rc", map[string]any{"id": "c2"}))
	k.mu.Unlock()
	must(a.events(ctx))
	if l := last(); l.Get("thread_ts") != "700.1" || !strings.Contains(l.Get("text"), "Cancelled") {
		t.Fatalf("follow-up = %v", l)
	}
}

func inputsOf(k *fakeKami, i int) map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.started[i]["inputs"].(map[string]any)
}

func TestBadRunThread(t *testing.T) {
	ctx := context.Background()
	k, s, start := setup(t)
	k.runs["rb"] = map[string]any{"id": "rb", "workflow": "ping", "vars": map[string]any{"thread_ts": "999.9"}}
	k.contracts["c1"] = &engine.Contract{ID: "c1", RunID: "rb", Origin: "state", Kind: "ask", Title: "Port?", Status: "open"}
	k.events = []eventbus.Event{
		{ID: 1, TS: time.Now(), Type: "run.say", Run: "rb", Data: map[string]any{"text": "hi"}},
		{ID: 2, TS: time.Now(), Type: "contract.opened", Run: "rb", Data: map[string]any{"id": "c1"}},
	}
	a := start()
	if err := a.events(ctx); err != nil {
		t.Fatal(err)
	}
	p := s.of("chat.postMessage")
	if a.st.Since != 2 || len(p) != 3 || p[2].Get("thread_ts") != "" || a.st.Threads["c1"].Thread != "" {
		t.Fatalf("bad thread blocked or misplaced: since %d posts %v", a.st.Since, p)
	}
}

func TestWorkerAskInRunThread(t *testing.T) {
	ctx := context.Background()
	k, s, start := setup(t)
	k.runs["rg"] = map[string]any{"id": "rg", "workflow": "goal", "vars": map[string]any{"thread_ts": "900.1"}}
	k.contracts["cw"] = &engine.Contract{ID: "cw", RunID: "rg", Origin: "worker", Kind: "ask", Title: "Which repo?", Status: "open"}
	k.events = []eventbus.Event{{ID: 1, TS: time.Now(), Type: "contract.opened", Run: "rg", Data: map[string]any{"id": "cw"}}}
	a := start()
	if err := a.events(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.onMessage(ctx, reply(owner, "900.1", "901.1", "the kami repo")); err != nil {
		t.Fatal(err)
	}
	// without --reply-answers the same reply goes to chat, with the thread's open contracts
	a.replyAnswers = false
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(a.onMessage(ctx, reply(owner, "900.1", "901.2", "how is it going?")))
	if len(k.resolves) != 1 || len(k.signals) != 1 || k.signals[0]["data"].(map[string]any)["thread_contracts"] != "cw" ||
		len(k.started) != 1 || inputsOf(k, 0)["thread_contracts"] != "cw" {
		t.Fatalf("chat routing: resolves %v signals %v started %v", k.resolves, k.signals, k.started)
	}
	p := s.of("chat.postMessage")
	if len(k.resolves) != 1 || k.resolves[0] != "cw=the kami repo map[]" ||
		p[len(p)-1].Get("thread_ts") != "900.1" || p[len(p)-1].Get("text") != "✓ Got it: the kami repo. POWER approves." {
		t.Fatalf("worker ask: resolves %v signals %v last %v", k.resolves, k.signals, p[len(p)-1])
	}
}

func TestCoworkers(t *testing.T) {
	ctx := context.Background()
	k, s, start := setup(t)
	a := start()
	a.coworkers, a.mrWorkflow = true, "mr"
	a.picture = filepath.Join(a.dir, "power.gif")
	os.MkdirAll(a.dir, 0o700)
	os.WriteFile(a.picture, []byte("GIF89a"), 0o600)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ch := func(user, ts, text string) *slackevents.MessageEvent {
		return &slackevents.MessageEvent{ChannelType: "channel", Channel: "C1", User: user, TimeStamp: ts, Text: text}
	}

	// an MR link in a channel: one question with two buttons in its thread; the app_mention twin is dropped
	link := "<https://gitlab.com/acme/web/-/merge_requests/42|!42>"
	must(a.onMessage(ctx, ch("UBOB", "10.1", "<@UPOWER> look "+link)))
	must(a.onCoworker(ctx, coMsg{"UBOB", "look " + link, "C1", "10.1", ""}))
	posts := s.of("chat.postMessage")
	if len(posts) != 1 || posts[0].Get("channel") != "C1" || posts[0].Get("thread_ts") != "10.1" ||
		!strings.Contains(posts[0].Get("blocks"), "power:review") || !strings.Contains(posts[0].Get("blocks"), "power:approve") {
		t.Fatalf("posts = %v", posts)
	}
	val, _ := json.Marshal(mrAsk{URL: "https://gitlab.com/acme/web/-/merge_requests/42", Requester: "UBOB", Channel: "C1", Thread: "10.1"})

	// someone else's tap does nothing; the requester's starts the mr workflow and redraws
	must(a.onMRTap(ctx, "UEVE", "C1", "100.1", "approve", string(val)))
	if len(k.started) != 0 {
		t.Fatal("a stranger's tap started a run")
	}
	must(a.onMRTap(ctx, "UBOB", "C1", "100.1", "approve", string(val)))
	if len(k.started) != 1 || k.started[0]["workflow"] != "mr" {
		t.Fatalf("started = %v", k.started)
	}
	if in := k.started[0]["inputs"].(map[string]any); in["action"] != "approve" || in["reply_ts"] != "10.1" || in["requester"] != "UBOB" ||
		in["url"] != "https://gitlab.com/acme/web/-/merge_requests/42" {
		t.Fatalf("inputs = %v", in)
	}
	if u := s.of("chat.update"); len(u) != 1 || strings.Contains(u[0].Get("blocks"), "power:") {
		t.Fatalf("redraw = %v", u)
	}

	// anything else: the picture once a day, then "no :)"; a non-owner DM counts too
	must(a.onMessage(ctx, reply("UBOB", "", "11.1", "hi")))
	must(a.onMessage(ctx, ch("UBOB", "12.1", "hello?")))
	if up := s.of("files.completeUploadExternal"); len(up) != 1 || up[0].Get("channel_id") != "D1" || up[0].Get("thread_ts") != "11.1" {
		t.Fatalf("uploads = %v", up)
	}
	if p := s.of("chat.postMessage"); len(p) != 2 || p[1].Get("text") != "no :)" || p[1].Get("thread_ts") != "12.1" {
		t.Fatalf("posts = %v", p)
	}
	must(a.onMessage(ctx, ch("UBOB", "12.2", "please")))
	must(a.onMessage(ctx, ch("UBOB", "12.3", "pretty please")))
	if p := s.of("chat.postMessage"); len(p) != 4 || p[2].Get("text") != "no :)" || p[3].Get("text") != "i only answer to the owner, bruh" {
		t.Fatalf("posts = %v", p)
	}

	// off: a non-owner DM is ignored again, the owner's DM still goes to chat
	a.coworkers = false
	must(a.onMessage(ctx, reply("UBOB", "", "13.1", "hi")))
	if len(s.of("chat.postMessage")) != 4 || len(s.of("files.completeUploadExternal")) != 1 {
		t.Fatal("coworker answered with the flag off")
	}
}
