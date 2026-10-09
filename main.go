// Command kami-slack is Kami's Slack surface. `serve` DMs the owner every
// contract as it opens and resolves it from a button or a thread reply; `dm`
// and `canvas` are for workflow script steps.
//
// It uses niffty's Slack app (tokens and owner id from niffty's .env) and
// replaces niffty: run one or the other, never both on the same app.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"kami/internal/engine"
)

const usage = `kami-slack — Kami's Slack surface (niffty's Slack app)

  kami-slack serve [--poll 5s] [--kekkai K]     contracts → owner DMs; buttons and thread replies resolve them;
                   [--chat-kekkai work] [--chat-workflow chat]   other owner DMs go to a chat run
                   [--coworkers=true] [--mr-workflow mr] [--picture PATH]   anyone else: an MR link gets
                                                review/approve buttons, anything else the picture (daily), then "no :)"
  kami-slack dm [--channel C] [--thread TS] [<text>...]   DM the owner, or post to C (text on stdin when no args); prints {"channel","ts"}
  kami-slack answer [--data JSON] <contract> <answer...>   answer a contract as the owner, from POWER's chat
                                                (refuses 🔒 contracts: those take a button press)
  kami-slack pin [--off] [--channel C] <ts>     pin (or unpin) a message, the owner DM by default
  kami-slack wrap <thread ts>                    unpin a chat thread and the status cards in it
  kami-slack canvas [--key <slug>] [--users U1,U2] <title>   markdown on stdin → private canvas shared with the owner (and users); prints its URL

  audit (every message in and out is in $KAMI_HOME/adapters/slack-transcript.ndjson):
  kami-slack thread <ts> [--json]               a thread of the owner DM, live from Slack, with its contract/run ids
  kami-slack threads [--since 24h] [--json]     threads in the transcript: root, first line, messages, last activity, runs, contracts
  kami-slack log [-f] [--run R] [--contract C] [--thread T] [--json]   the transcript, filtered; -f follows

  common flags: --slack-env PATH (default ~/.local/share/niffty/.env)  --kami-sock PATH`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	h, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("kami-slack "+cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	envPath := fs.String("slack-env", filepath.Join(h, ".local/share/niffty/.env"), "file holding SLACK_BOT_TOKEN, SLACK_APP_TOKEN, NIFFTY_OWNER_ID")
	sock := fs.String("kami-sock", kamiSock(), "kami daemon socket")
	poll := fs.Duration("poll", 5*time.Second, "serve: kami event poll interval")
	kek := fs.String("kekkai", "", "serve: only contracts from this kekkai")
	key := fs.String("key", "", "canvas: stable name; the same key edits the same canvas")
	off := fs.Bool("off", false, "pin: unpin instead")
	data := fs.String("data", "", "answer: form fields as a JSON object")
	channel := fs.String("channel", "", "dm: post to this channel instead of the owner DM")
	thread := fs.String("thread", "", "dm: post into this thread (its root message ts); log: only this thread")
	asJSON := fs.Bool("json", false, "thread, threads, log: JSON output")
	follow := fs.Bool("f", false, "log: keep printing new entries")
	run := fs.String("run", "", "log: only entries of this run")
	contract := fs.String("contract", "", "log: only entries of this contract")
	since := fs.Duration("since", 24*time.Hour, "threads: active within this long")
	chatKek := fs.String("chat-kekkai", "work", "serve: kekkai that owns chat runs")
	chatWf := fs.String("chat-workflow", "chat", "serve: workflow started for a DM; empty disables chat routing")
	replyAnswers := fs.Bool("reply-answers", false, "serve: a thread reply answers its open contract directly instead of going to chat")
	coworkers := fs.Bool("coworkers", true, "serve: answer other people (DMs, mentions, channels POWER is in)")
	mrWf := fs.String("mr-workflow", "mr", "serve: workflow (in --chat-kekkai) a coworker's review/approve tap starts")
	picture := fs.String("picture", filepath.Join(kamiHome(), "adapters", "power.gif"), "serve: image sent to coworkers once a day")
	canvasUsers := fs.String("users", "", "canvas: extra Slack user ids (comma-separated) who may open it")
	fs.Parse(args)
	rest := fs.Args()
	if cmd == "thread" || cmd == "threads" || cmd == "log" { // flags may follow the ts
		rest = nil
		for fs.NArg() > 0 {
			rest = append(rest, fs.Arg(0))
			fs.Parse(fs.Args()[1:])
		}
	}
	dir := filepath.Join(kamiHome(), "adapters")
	switch cmd { // transcript only: no Slack credentials needed
	case "log":
		err := printLog(context.Background(), os.Stdout, filepath.Join(dir, "slack-transcript.ndjson"),
			filter{run: *run, contract: *contract, thread: *thread}, *follow, *asJSON)
		if err != nil {
			log.Fatalf("kami-slack log: %v", err)
		}
		return
	case "threads":
		entries, err := readTranscript(filepath.Join(dir, "slack-transcript.ndjson"))
		if err == nil {
			err = printThreads(os.Stdout, summarize(entries, time.Now().Add(-*since)), *asJSON)
		}
		if err != nil {
			log.Fatalf("kami-slack threads: %v", err)
		}
		return
	}

	env, err := dotenv(*envPath, "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "NIFFTY_OWNER_ID")
	if err != nil {
		log.Fatalf("kami-slack: %v", err)
	}
	if env["SLACK_BOT_TOKEN"] == "" || env["NIFFTY_OWNER_ID"] == "" {
		log.Fatalf("kami-slack: SLACK_BOT_TOKEN and NIFFTY_OWNER_ID must be set in %s", *envPath)
	}
	a := &adapter{
		api:          slack.New(env["SLACK_BOT_TOKEN"], slack.OptionAppLevelToken(env["SLACK_APP_TOKEN"])),
		kami:         kamiClient(*sock),
		owner:        env["NIFFTY_OWNER_ID"],
		kekkai:       *kek,
		chatKekkai:   *chatKek,
		chatWorkflow: *chatWf,
		coworkers:    *coworkers,
		replyAnswers: *replyAnswers,
		mrWorkflow:   *mrWf,
		picture:      *picture,
		dir:          dir,
	}
	if cmd != "serve" {
		a.run = os.Getenv("KAMI_RUN") // set for a workflow script step
	}
	ctx := context.Background()
	switch cmd {
	case "serve":
		if env["SLACK_APP_TOKEN"] == "" {
			log.Fatalf("kami-slack: SLACK_APP_TOKEN must be set in %s for serve", *envPath)
		}
		err = a.serve(ctx, *poll)
	case "dm":
		text := strings.Join(fs.Args(), " ")
		if text == "" {
			raw, _ := io.ReadAll(os.Stdin)
			text = string(raw)
		}
		if strings.TrimSpace(text) == "" {
			log.Fatal("kami-slack dm: no text")
		}
		kind := "dm"
		if *thread != "" {
			kind = "thread_reply"
		}
		var ch, ts string
		if ch, ts, err = a.post(ctx, entry{Kind: kind, Channel: *channel, ThreadTS: *thread, Text: strings.TrimSpace(text)}, nil); err == nil {
			json.NewEncoder(os.Stdout).Encode(map[string]string{"channel": ch, "ts": ts})
		}
	case "canvas":
		title := strings.Join(fs.Args(), " ")
		if title == "" {
			log.Fatal("kami-slack canvas: a title is required")
		}
		raw, _ := io.ReadAll(os.Stdin)
		var u string
		if u, err = a.canvas(ctx, *key, title, string(raw), splitIDs(*canvasUsers)); err == nil {
			fmt.Println(u)
		}
	case "answer":
		if fs.NArg() < 2 {
			log.Fatal("kami-slack answer: a contract id and an answer are required")
		}
		var d map[string]any
		if *data != "" {
			if err := json.Unmarshal([]byte(*data), &d); err != nil {
				log.Fatalf("kami-slack answer: --data: %v", err)
			}
		}
		err = a.answer(fs.Arg(0), strings.Join(fs.Args()[1:], " "), d)
	case "wrap":
		if fs.NArg() != 1 {
			log.Fatal("kami-slack wrap: one thread ts is required")
		}
		err = a.wrap(ctx, fs.Arg(0))
	case "pin":
		if fs.NArg() != 1 {
			log.Fatal("kami-slack pin: one message ts is required")
		}
		err = a.pin(ctx, *channel, fs.Arg(0), !*off)
	case "thread":
		if len(rest) != 1 {
			log.Fatal("kami-slack thread: one thread ts is required")
		}
		err = a.printThread(ctx, os.Stdout, rest[0], *asJSON)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("kami-slack %s: %v", cmd, err)
	}
}

func kamiHome() string {
	if h := os.Getenv("KAMI_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".kami")
}

func kamiSock() string {
	if s := os.Getenv("KAMI_SOCK"); s != "" {
		return s
	}
	return filepath.Join(kamiHome(), "kami.sock")
}

// dotenv reads only the named keys from a KEY=value file. The values are
// secrets: they are never logged or printed.
func dotenv(path string, keys ...string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		k = strings.TrimSpace(k)
		if ok && contains(keys, k) {
			out[k] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out, nil
}

func kamiClient(sock string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
}

type adapter struct {
	api    *slack.Client
	kami   *http.Client
	owner  string
	kekkai string
	dir    string // $KAMI_HOME/adapters

	chatKekkai, chatWorkflow string
	coworkers                bool            // answer people other than the owner (feature flag)
	replyAnswers             bool            // a thread reply answers its open contract; off: it goes to chat
	mrWorkflow, picture      string          // started on a coworker's review/approve tap; sent to anything else
	recent                   map[string]bool // channel/ts already handled: a mention arrives as two events
	im                       string          // the owner's DM channel, opened once
	st                       state
	run                      string          // $KAMI_RUN of a script step's dm/canvas, for the transcript
	links                    map[string]link // run id -> its thread, once known
}

// --- Slack ---------------------------------------------------------------------

// post sends e.Text to e.Channel (the owner's DM when empty), threaded under
// e.ThreadTS when set, and records e in the transcript (as kind error when the
// post fails). Blocks are optional; text is always sent as the notification fallback.
func (a *adapter) post(ctx context.Context, e entry, blocks []slack.Block) (string, string, error) {
	e.Dir = "out"
	im, err := e.Channel, error(nil)
	if im == "" {
		im, err = a.dm(ctx)
	}
	if err == nil {
		opts := []slack.MsgOption{slack.MsgOptionText(e.Text, false)}
		if blocks != nil {
			opts = append(opts, slack.MsgOptionBlocks(blocks...))
		}
		if e.ThreadTS != "" {
			opts = append(opts, slack.MsgOptionTS(e.ThreadTS))
		}
		if e.Channel, e.TS, err = a.api.PostMessageContext(ctx, im, opts...); err != nil {
			err = fmt.Errorf("post message: %w", err)
		}
	}
	if err != nil {
		e.Kind, e.Error = "error", strings.TrimPrefix(e.Error+"; ", "; ")+err.Error()
		a.record(e)
		return "", "", err
	}
	a.record(e)
	return e.Channel, e.TS, nil
}

// pin pins or unpins a message. Already pinned / not pinned counts as done.
func (a *adapter) pin(ctx context.Context, channel, ts string, on bool) error {
	var err error
	if channel == "" {
		channel, err = a.dm(ctx)
	}
	e := entry{Dir: "out", Kind: "pin", Channel: channel, TS: ts, Text: "pin"}
	if err == nil {
		ref := slack.NewRefToMessage(channel, ts)
		if on {
			err = a.api.AddPinContext(ctx, channel, ref)
		} else {
			e.Text = "unpin"
			err = a.api.RemovePinContext(ctx, channel, ref)
		}
		var se slack.SlackErrorResponse
		if errors.As(err, &se) && (se.Err == "already_pinned" || se.Err == "no_pin") {
			err = nil
		}
	}
	if err != nil {
		e.Kind, e.Error = "error", e.Text+": "+err.Error()
	}
	a.record(e)
	return err
}

// dm returns the owner's DM channel, opening it on first use.
func (a *adapter) dm(ctx context.Context) (string, error) {
	if a.im == "" {
		ch, _, _, err := a.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: []string{a.owner}})
		if err != nil {
			return "", fmt.Errorf("open DM with owner: %w", err)
		}
		a.im = ch.ID
	}
	return a.im, nil
}

var slug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// canvas creates a canvas only the bot and the owner can see, or with a key
// edits the canvas that key made before; it returns the permalink.
// A key's canvas id lives in its own file, so parallel script steps never race.
func (a *adapter) canvas(ctx context.Context, key, title, markdown string, users []string) (string, error) {
	u, err := a.writeCanvas(ctx, key, title, markdown, users)
	e := entry{Dir: "out", Kind: "canvas", Text: markdown, Title: title, CanvasURL: u}
	if err != nil {
		e.Kind, e.Error = "error", err.Error()
	}
	a.record(e)
	return u, err
}

func (a *adapter) writeCanvas(ctx context.Context, key, title, markdown string, users []string) (string, error) {
	doc := slack.DocumentContent{Type: "markdown", Markdown: markdown}
	var keyFile string
	if key != "" {
		if !slug.MatchString(key) {
			return "", fmt.Errorf("--key %q: use letters, digits, '.', '_' or '-'", key)
		}
		keyFile = filepath.Join(a.dir, "slack-canvas", key)
		if raw, err := os.ReadFile(keyFile); err == nil {
			id := strings.TrimSpace(string(raw))
			err := a.api.EditCanvasContext(ctx, slack.EditCanvasParams{CanvasID: id,
				Changes: []slack.CanvasChange{{Operation: "replace", DocumentContent: doc}}})
			if err == nil {
				return a.permalink(ctx, id)
			}
			log.Printf("kami-slack: canvas %s for key %s unreachable (%v); making a new one", id, key, err)
		}
	}
	id, err := a.api.CreateCanvasContext(ctx, title, doc)
	if err != nil {
		return "", fmt.Errorf("create canvas: %w", err)
	}
	// Without access the owner's permalink 404s; no channel is ever granted.
	if err := a.api.SetCanvasAccessContext(ctx, slack.SetCanvasAccessParams{CanvasID: id, AccessLevel: "write", UserIDs: append([]string{a.owner}, users...)}); err != nil {
		return "", fmt.Errorf("grant canvas access: %w", err)
	}
	if keyFile != "" {
		if err := writeAtomic(keyFile, []byte(id+"\n")); err != nil {
			return "", err
		}
	}
	return a.permalink(ctx, id)
}

func (a *adapter) permalink(ctx context.Context, id string) (string, error) {
	f, _, _, err := a.api.GetFileInfoContext(ctx, id, 0, 0)
	if err != nil {
		return "", fmt.Errorf("permalink for %s: %w", id, err)
	}
	return f.Permalink, nil
}

// answer resolves a contract with the owner's words, relayed by the chat run
// in that thread. A human-only contract (gate, action, lock) refuses: only
// the owner's own button press may answer it. serve posts the follow-up.
func (a *adapter) answer(id, answer string, data map[string]any) error {
	var c engine.ContractView
	if err := a.kamiCall("GET", "/contracts/"+url.PathEscape(id), nil, &c); err != nil {
		return err
	}
	e := entry{Dir: "in", Kind: "chat_answer", Text: answer, ContractID: id, RunID: c.RunID, Workflow: c.Workflow}
	if c.Lock {
		e.Error = "locked"
		a.record(e)
		return fmt.Errorf("contract %s is 🔒: only the owner's button press answers it", id)
	}
	err := a.kamiCall("POST", "/contracts/"+url.PathEscape(id)+"/resolve",
		map[string]any{"answer": answer, "data": data, "note": "via POWER chat, from the owner's words"}, nil)
	if err != nil {
		e.Error = err.Error()
	}
	a.record(e)
	return err
}

// --- Kami ----------------------------------------------------------------------

// kamiErr is a refusal from the daemon (bad answer, conflict), shown to the user.
type kamiErr struct{ engine.APIError }

func (e *kamiErr) Error() string { return e.Code + ": " + e.Message }

func (a *adapter) kamiCall(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, "http://kami"+path, rd)
	resp, err := a.kami.Do(req) // never X-Kami-Token: that marks a worker, which may not resolve
	if err != nil {
		return fmt.Errorf("kami: %v", err)
	}
	defer resp.Body.Close()
	var env engine.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("kami %s %s: %v", method, path, err)
	}
	if !env.OK {
		if env.Error == nil {
			return fmt.Errorf("kami %s %s: HTTP %d", method, path, resp.StatusCode)
		}
		if env.Error.Code == "INTERNAL" || env.Error.Code == "UNAVAILABLE" {
			return fmt.Errorf("kami %s %s: %s", method, path, env.Error.Message)
		}
		return &kamiErr{*env.Error}
	}
	if out != nil {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

// --- state ---------------------------------------------------------------------

type state struct {
	Since   int64               `json:"since"`           // last kami event handled
	Threads map[string]*thread  `json:"threads"`         // contract id -> its DM
	Queue   map[string][]queued `json:"queue,omitempty"` // chat thread key -> messages not yet handed to kami
	Seen    map[string]string   `json:"seen,omitempty"`  // coworker user -> local day POWER last sent her picture
	Count   map[string]int      `json:"count,omitempty"` // coworker user -> messages that day (other than MR links)
	Cards   map[string]*card    `json:"cards,omitempty"` // top-level run id -> its status card
}

type queued struct {
	Text string `json:"text"`
	TS   string `json:"ts"`
}

type thread struct {
	TS      string `json:"ts"`               // the contract message's ts
	Thread  string `json:"thread,omitempty"` // its run's thread when it was posted into one; else replies thread under TS
	Text    string `json:"text"`             // what was posted, to redraw it without buttons
	Buttons bool   `json:"buttons"`          // answered by button
	Mine    bool   `json:"mine,omitempty"`   // resolved from Slack: no follow-up for contract.resolved

	RunID    string `json:"run_id,omitempty"` // for the transcript
	Workflow string `json:"workflow,omitempty"`
}

func (a *adapter) statePath() string { return filepath.Join(a.dir, "slack.json") }

func (a *adapter) load() error {
	a.st = state{Threads: map[string]*thread{}, Queue: map[string][]queued{}, Seen: map[string]string{}, Count: map[string]int{}, Cards: map[string]*card{}}
	raw, err := os.ReadFile(a.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &a.st); err != nil {
		return fmt.Errorf("%s: %v", a.statePath(), err)
	}
	if a.st.Threads == nil {
		a.st.Threads = map[string]*thread{}
	}
	if a.st.Queue == nil {
		a.st.Queue = map[string][]queued{}
	}
	if a.st.Seen == nil {
		a.st.Seen = map[string]string{}
	}
	if a.st.Count == nil {
		a.st.Count = map[string]int{}
	}
	if a.st.Cards == nil {
		a.st.Cards = map[string]*card{}
	}
	return nil
}

func (a *adapter) save() error {
	raw, _ := json.MarshalIndent(a.st, "", "  ")
	return writeAtomic(a.statePath(), raw)
}

// writeAtomic writes then renames, so a crash never leaves half a file.
func writeAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func splitIDs(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
