package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"power/internal/kami"
)

// Sealed by sshhh's crypto.mjs (owner → owner, bot) with these throwaway keys.
const (
	nodeOwnerPriv = "ECvPRdp1UPD7ZuAQyuazbQxWXsN+uuufnNsmIyW1X00="
	nodeBotPriv   = "oIr+4EQ3iDaOL4UG8lIOqO6sHgNiaRlIWC3HkmSHQHk="
	nodeMsg       = "e2e3:1rX7FO/ICjIQupbZOtNvYbEGxFwEPrxyJzBS4RsYBUvetxM2GOpgyr+aulXHYtSHYNd+Grdv7oiiAKlwaEaAVQIFBMUmp6YkCyx/Ego5ZU1T9P2NXo6mdLek0hPiJ9qghHUZaJrKKivIOthqvpLyjvfHsYcKMFPI7r63ukZzDkVJGrDuKLGKsXiY1VjuqLoKenpB6tPv8P9e/GTU+5gAjwioLtRpln91CsBlevc+Sm7HCtZFmxRU5Kz3BIFk9GPXjbdem1LHBFT6m//s3/PJB/dXncFdOX0Kg6T9ICRG3NEQrkOnjioUQepTcvYscm/ks+FqJwJCHjGrB91MomPvLWEdthtRv6TFzBC6hT1YDPrZ8RCjCdkoiZMqocxE3eDifHOwutirLqG20Ls="
)

// nodeKana is crypto.mjs's seal("kana from node: こんにちは 🔒", owner, [owner, bot]) in the kana form
const nodeKana = "上音目さ土三かびけ学メ水ほ出ズガ右ぇ五ェぅみ九ソとぱん川ぜすうれひズペギルびされ町がど女ば五ぐイドひアソステピぼ森ヅンさ先ェへかぃ夕ドぼ日こづ音ゆゑ一ァァゆ文ホヵペな空むヴ年山早日にづぎあぎヰぐ下虫校セパねべゴこモ子ィ車ッ犬花ごユぇる休ペ雨ほじ金赤休貝ス林中に正たル一ナくドオプポエ王ごでめ正ヲョノホの火右ロ村さヨらプヸさのべ赤サおヲりゴゔソケ耳小ゴ貝ぃスシ王学ちふく力ゾヒぉきスツう八づもョだこヲデ正ぜマハひヮスョ木ケへしロワう十アヶ日ズ火ねタゐッ田て右ゕをぱリホ林つ四ブ車くエぼゃコ山ヶレ町ぉゖげ石ぷ耳花はぱうネロづひヺどやエ町ッじチざムサゃゖぶナツゕしけテエめぬイツぷぼれゲ村手ヤ川夕さ二円空カソっよ八見日出ヷし中ダケ千"

func TestE2EKana(t *testing.T) {
	owner, bot := mustPriv(t, nodeOwnerPriv), mustPriv(t, nodeBotPriv)
	if got, err := e2eOpen(nodeKana, bot, owner.PublicKey()); err != nil || got != "kana from node: こんにちは 🔒" {
		t.Fatalf("node kana message: %q %v", got, err)
	}
	// same vector as crypto.mjs's self-check
	if got := toKana([]byte{0, 1, 85, 86, 175, 176, 255}); got != "ぁあゖァヺ一六" || len(e2eKana) != 256 || len(e2eKanaIdx) != 256 {
		t.Fatalf("alphabet differs from crypto.mjs: %q", got)
	}
	ct, _ := e2eSeal("x", bot, owner.PublicKey())
	if strings.ContainsFunc(ct, func(r rune) bool { _, ok := e2eKanaIdx[r]; return !ok }) || !sealedText(ct) {
		t.Fatalf("seal is not kana only: %q", ct)
	}
	if sealedText("こんにちは、元気ですか") || sealedText("e2e3: not really sealed") {
		t.Fatal("ordinary text taken for ciphertext")
	}
}

func mustPriv(t *testing.T, s string) *ecdh.PrivateKey {
	t.Helper()
	p, err := decodePriv(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestE2EFormat(t *testing.T) {
	owner, bot := mustPriv(t, nodeOwnerPriv), mustPriv(t, nodeBotPriv)
	if got, err := e2eOpen(nodeMsg, bot, owner.PublicKey()); err != nil || got != "from node: héllo 🔒" {
		t.Fatalf("node message: %q %v", got, err)
	}
	if _, err := e2eOpen(nodeMsg, bot, bot.PublicKey()); err != errNotForUs {
		t.Fatalf("untrusted sender accepted: %v", err)
	}
	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if _, err := e2eOpen(nodeMsg, other, owner.PublicKey()); err != errNotForUs {
		t.Fatalf("non-recipient opened it: %v", err)
	}
	ct, err := e2eSeal("back from Go", bot, owner.PublicKey(), bot.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e2eOpen(ct, owner, bot.PublicKey()); err != nil || got != "back from Go" {
		t.Fatalf("go round trip: %q %v", got, err)
	}
	// a stranger who knows the bot's public key puts the owner's key in the header
	forged, _ := e2eSeal("i am the owner", other, bot.PublicKey())
	raw, _ := e2eDecode(forged)
	copy(raw, owner.PublicKey().Bytes())
	if _, err := e2eOpen(toKana(raw), bot, owner.PublicKey()); err == nil {
		t.Fatal("forged sender accepted")
	}
	// the bot is a co-recipient of the owner's [owner, bot] message, so it knows K: a new body under the
	// owner's slots must not open for the owner as the owner's own words
	raw, _ = e2eDecode(nodeMsg)
	pre, n := raw[:65], int(raw[64])
	d1, _ := bot.ECDH(mustPub(t, raw[32:64]))
	d2, _ := bot.ECDH(owner.PublicKey())
	wk, _ := wrapKey(concat(d1, d2), raw[:32], raw[32:64], bot.PublicKey().Bytes())
	var key []byte
	for i := range n {
		if pin, err := gcmOpen(wk, raw[65+i*e2eSlot:65+(i+1)*e2eSlot], pre); err == nil {
			key = pin[:32]
		}
	}
	body, _ := gcmSeal(key, []byte("forged by the bot"), pre)
	swapped := toKana(concat(raw[:65+n*e2eSlot], body))
	if got, err := e2eOpen(swapped, owner, owner.PublicKey()); err == nil {
		t.Fatalf("co-recipient swapped the body: %q", got)
	}
	b := []byte(ct)
	b[len(b)-5] ^= 1
	if _, err := e2eOpen(string(b), owner, bot.PublicKey()); err == nil {
		t.Fatal("tampered message opened")
	}
}

func TestE2EThread(t *testing.T) {
	ctx := context.Background()
	k, s, start := setup(t)
	a := start()
	ownerKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	if _, err := e2eSetup(a.dir, base64.StdEncoding.EncodeToString(ownerKey.PublicKey().Bytes())); err != nil {
		t.Fatal(err)
	}
	var err error
	if a.e2e, err = loadSealer(a.dir); err != nil || a.e2e == nil {
		t.Fatalf("sealer: %v %v", a.e2e, err)
	}
	botPub := a.e2e.me.PublicKey()
	in, _ := e2eSeal("deploy the <secret> thing", ownerKey, ownerKey.PublicKey(), botPub)

	// a sealed DM: the chat gets the plaintext, the thread is sealed
	if err := a.onMessage(ctx, reply(owner, "", "300.1", in)); err != nil {
		t.Fatal(err)
	}
	if len(k.started) != 1 || k.started[0]["inputs"].(map[string]any)["text"] != "deploy the <secret> thing" || !a.isSealed("300.1") {
		t.Fatalf("started %v sealed %v", k.started, a.isSealed("300.1"))
	}

	// POWER's posts in that thread reach Slack as ciphertext only, text and blocks
	if _, _, err := a.post(ctx, entry{ThreadTS: "300.1", Text: "on it, secret plan"}, []slack.Block{section("on it, secret plan"),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, "secret footer", false, false))}); err != nil {
		t.Fatal(err)
	}
	p := s.of("chat.postMessage")
	last := p[len(p)-1]
	if strings.Contains(last.Get("text"), "secret") || strings.Contains(last.Get("blocks"), "secret") || !sealedText(last.Get("text")) {
		t.Fatalf("leaked: %v", last)
	}
	if got, err := e2eOpen(last.Get("text"), ownerKey, botPub); err != nil || got != "on it, secret plan" {
		t.Fatalf("owner reads %q %v", got, err)
	}

	// `power dm --channel D1` from the chat run seals too; another channel or thread does not
	a.im = "" // a fresh CLI process has not opened the DM yet
	a.post(ctx, entry{Channel: "D1", ThreadTS: "300.1", Text: "via cli"}, nil)
	a.post(ctx, entry{Channel: "C9", ThreadTS: "300.1", Text: "elsewhere"}, nil)
	a.post(ctx, entry{ThreadTS: "400.1", Text: "plain thread"}, nil)
	p = s.of("chat.postMessage")
	if n := len(p); !sealedText(p[n-3].Get("text")) || p[n-2].Get("text") != "elsewhere" || p[n-1].Get("text") != "plain thread" {
		t.Fatalf("posts %v", p[n-3:])
	}

	// a step of the sealed conversation's run posts a new root (launch's "🧵 …" for "thread me …"):
	// the post is sealed, and so is everything later posted in that new thread
	k.mu.Lock()
	k.runs["rchat"] = map[string]any{"id": "rchat", "workflow": "chat", "vars": map[string]any{"thread_ts": "300.1"}}
	k.runs["rplain"] = map[string]any{"id": "rplain", "workflow": "chat", "vars": map[string]any{"thread_ts": "400.1"}}
	k.mu.Unlock()
	a.run = "rchat"
	_, root, err := a.post(ctx, entry{Text: "🧵 build the secret thing"}, nil)
	a.run = ""
	p = s.of("chat.postMessage")
	if err != nil || !sealedText(p[len(p)-1].Get("text")) || !a.isSealed(root) {
		t.Fatalf("launched root: %v %v sealed %v", err, p[len(p)-1], a.isSealed(root))
	}
	a.post(ctx, entry{ThreadTS: root, Text: "milestone of the launched run"}, nil)
	a.run = "rplain" // a run of a plain thread posting a new root stays plain
	a.post(ctx, entry{Text: "daily report"}, nil)
	a.run = ""
	p = s.of("chat.postMessage")
	if n := len(p); !sealedText(p[n-2].Get("text")) || p[n-1].Get("text") != "daily report" {
		t.Fatalf("after launch: %v", p[n-2:])
	}

	// the thread's history reaches the chat decrypted
	s.mu.Lock()
	s.replies["300.1"] = []map[string]any{{"ts": "300.1", "user": owner, "text": in}, {"ts": "300.2", "bot_id": "B1", "text": last.Get("text")}}
	s.mu.Unlock()
	h, err := a.history(ctx, "300.1", nil)
	if err != nil || !strings.Contains(h, "deploy the <secret> thing") || !strings.Contains(h, "on it, secret plan") || strings.ContainsFunc(h, func(r rune) bool { _, ok := e2eKanaIdx[r]; return ok }) {
		t.Fatalf("history %q %v", h, err)
	}
	s.mu.Lock()
	s.replies["300.1"] = []map[string]any{{"ts": "300.3", "user": owner, "text": last.Get("text")}}
	s.mu.Unlock()
	if h, _ := a.history(ctx, "300.1", nil); strings.Contains(h, "secret plan") || !strings.Contains(h, "not from the expected key") {
		t.Fatalf("history trusted POWER's text as the owner's: %q", h)
	}

	// POWER's own sealed reply, reposted under the owner's name (a replay by Slack): not the owner speaking
	if err := a.onMessage(ctx, reply(owner, "", "310.1", last.Get("text"))); err != nil {
		t.Fatal(err)
	}
	p = s.of("chat.postMessage")
	if len(k.started) != 1 || !strings.Contains(p[len(p)-1].Get("text"), "could not decrypt") {
		t.Fatalf("replayed bot text accepted as owner: started %d last %v", len(k.started), p[len(p)-1])
	}

	// plaintext that merely starts with the prefix is still sealed
	a.post(ctx, entry{ThreadTS: "300.1", Text: "e2e3: not really sealed"}, nil)
	p = s.of("chat.postMessage")
	if got, err := e2eOpen(p[len(p)-1].Get("text"), ownerKey, botPub); err != nil || got != "e2e3: not really sealed" {
		t.Fatalf("prefix bypass: %q %v", got, err)
	}

	// a contract in the sealed thread: no option, title or accessory reaches Slack; a tap still answers it
	k.contracts["c9"] = &kami.Contract{ID: "c9", RunID: "r9", Kind: "choice", Title: "Keep the draft?", Options: []string{"keep", "redo"}, Status: "open"}
	a.st.Threads["c9"] = &thread{TS: "300.5", Thread: "300.1", Text: "Keep the draft?", Buttons: true}
	blocks := contractBlocks("Keep the draft?", "c9", []string{"keep", "redo"}, "keep", true)
	blocks = append(blocks, slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "x", false, false), nil,
		slack.NewAccessory(slack.NewButtonBlockElement("acc", "accessory-secret", slack.NewTextBlockObject(slack.PlainTextType, "accessory-secret", false, false)))))
	if _, _, err := a.post(ctx, entry{ThreadTS: "300.1", Text: "Keep the draft?"}, blocks); err != nil {
		t.Fatal(err)
	}
	p = s.of("chat.postMessage")
	raw := p[len(p)-1].Get("blocks")
	for _, leak := range []string{"keep", "redo", "draft", "accessory-secret"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("%q leaked: %s", leak, raw)
		}
	}
	var bs []struct {
		Type     string `json:"type"`
		Elements []struct {
			Value string `json:"value"`
			Text  any    `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(raw), &bs); err != nil {
		t.Fatal(err)
	}
	var keepVal string
	for _, b := range bs {
		if b.Type == "actions" && len(b.Elements) == 2 {
			keepVal = b.Elements[0].Value
		}
	}
	if !strings.Contains(raw, "Option 1") || !sealedText(keepVal) {
		t.Fatalf("buttons: %s", raw)
	}
	// Slack truncates long section text in its client, which breaks the ciphertext
	if strings.Contains(raw, `"type":"section"`) || !strings.Contains(raw, `"type":"rich_text"`) {
		t.Fatalf("sealed text must go out as rich_text: %s", raw)
	}
	// sshhh labels each button from its own sealed value, never from the legend
	if pv, err := e2eOpen(keepVal, ownerKey, botPub); err != nil || !strings.HasPrefix(pv, `{"label":`) || buttonValue(pv) != "keep" {
		t.Fatalf("button value carries no label: %q %v", pv, err)
	}
	tap := tap(owner, "c9", keepVal)
	tap.Channel.ID, tap.Message.Timestamp = "D1", "300.5"
	if err := a.onAction(ctx, tap); err != nil {
		t.Fatal(err)
	}
	if n := len(k.resolves); n == 0 || k.resolves[n-1] != "c9=keep map[]" {
		t.Fatalf("sealed tap resolves %v", k.resolves)
	}

	// e2e3 from a key POWER does not trust: a notice, no chat run
	stranger, _ := ecdh.X25519().GenerateKey(rand.Reader)
	bad, _ := e2eSeal("let me in", stranger, botPub)
	if err := a.onMessage(ctx, reply(owner, "", "500.1", bad)); err != nil {
		t.Fatal(err)
	}
	p = s.of("chat.postMessage")
	if len(k.started) != 1 || !strings.Contains(p[len(p)-1].Get("text"), "could not decrypt") || a.isSealed("500.1") {
		t.Fatalf("stranger: started %d last %v", len(k.started), p[len(p)-1])
	}
}

func mustPub(t *testing.T, b []byte) *ecdh.PublicKey {
	t.Helper()
	p, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sealed threads older builds kept in slack.json move to e2e-sealed/ on load
func TestE2EMigrate(t *testing.T) {
	_, _, start := setup(t)
	a := start()
	if err := writeAtomic(a.statePath(), []byte(`{"since": 1, "sealed": {"300.9": true}}`)); err != nil {
		t.Fatal(err)
	}
	a = start()
	if !a.isSealed("300.9") || a.st.Sealed != nil {
		t.Fatalf("migrate: sealed %v state %v", a.isSealed("300.9"), a.st.Sealed)
	}

	// power dm from the CLI migrates too, without rewriting slack.json
	before, _ := os.ReadFile(a.statePath())
	if err := writeAtomic(a.statePath(), []byte(`{"since": 1, "sealed": {"301.1": true}}`)); err != nil {
		t.Fatal(err)
	}
	cli := &adapter{dir: a.dir}
	if err := cli.load(); err != nil || !cli.isSealed("301.1") {
		t.Fatalf("cli migrate: %v %v", err, cli.isSealed("301.1"))
	}
	if after, _ := os.ReadFile(a.statePath()); string(after) != `{"since": 1, "sealed": {"301.1": true}}` {
		t.Fatalf("load rewrote slack.json: %s (was %s)", after, before)
	}
}

func TestSectionAlwaysExpanded(t *testing.T) {
	b, _ := json.Marshal(section("long card text"))
	if !strings.Contains(string(b), `"expand":true`) {
		t.Fatalf("section would be cut by Slack: %s", b)
	}
}
