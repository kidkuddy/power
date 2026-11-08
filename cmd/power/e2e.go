package main

// End-to-end encrypted owner threads (github.com/kidkuddy/sshhh). A thread the
// owner writes into with an e2e3: message is sealed: everything POWER posts or
// redraws in it goes to Slack as ciphertext only, readable by the owner's Mac
// and by POWER. The format is sshhh's crypto.mjs; keep the two in step.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

const (
	e2ePrefix = "e2e3:"
	e2eSlot   = 12 + 64 + 16 // iv | K || SHA-256(body) | tag
)

// e2eKeys is $KAMI_HOME/adapters/e2e.json: POWER's private key and the owner's public key.
type e2eKeys struct {
	Key   string `json:"key"`   // base64 X25519 private key: a secret, never logged
	Owner string `json:"owner"` // base64 X25519 public key of the owner's Mac
	// SealKekkai: kekkais whose runs only ever post sealed: a run with no thread yet opens a
	// sealed one, and a post that cannot be sealed is refused instead of sent in plaintext.
	SealKekkai []string `json:"seal_kekkai,omitempty"`
}

type sealer struct {
	me     *ecdh.PrivateKey
	owner  *ecdh.PublicKey
	kekkai map[string]bool // e2eKeys.SealKekkai
}

func e2ePath(dir string) string { return filepath.Join(dir, "e2e.json") }

// loadSealer is nil without e2e.json or an owner key: sealing is off.
func loadSealer(dir string) (*sealer, error) {
	raw, err := os.ReadFile(e2ePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var k e2eKeys
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("%s: %v", e2ePath(dir), err)
	}
	if k.Owner == "" {
		return nil, nil
	}
	me, err := decodePriv(k.Key)
	if err != nil {
		return nil, fmt.Errorf("%s key: %v", e2ePath(dir), err)
	}
	owner, err := decodePub(k.Owner)
	if err != nil {
		return nil, fmt.Errorf("%s owner: %v", e2ePath(dir), err)
	}
	kek := map[string]bool{}
	for _, k := range k.SealKekkai {
		kek[k] = true
	}
	return &sealer{me, owner, kek}, nil
}

// e2eSetup makes POWER's key on first use, records the owner's key when given,
// and returns POWER's public key for the owner's sshhh (E2E_PEERS).
func e2eSetup(dir, ownerPub string) (string, error) {
	var k e2eKeys
	if raw, err := os.ReadFile(e2ePath(dir)); err == nil {
		if err := json.Unmarshal(raw, &k); err != nil {
			return "", fmt.Errorf("%s: %v", e2ePath(dir), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if k.Key == "" {
		p, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return "", err
		}
		k.Key = base64.StdEncoding.EncodeToString(p.Bytes())
	}
	if ownerPub != "" {
		if _, err := decodePub(ownerPub); err != nil {
			return "", fmt.Errorf("owner key: %v", err)
		}
		k.Owner = ownerPub
	}
	me, err := decodePriv(k.Key)
	if err != nil {
		return "", err
	}
	raw, _ := json.MarshalIndent(k, "", "  ")
	if err := writeAtomic(e2ePath(dir), raw); err != nil { // 0600
		return "", err
	}
	return base64.StdEncoding.EncodeToString(me.PublicKey().Bytes()), nil
}

func decodePriv(s string) (*ecdh.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(b)
}

func decodePub(s string) (*ecdh.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPublicKey(b)
}

// --- format (e2e3, see crypto.mjs) -------------------------------------------
//
// kana( pre | n × slot | body ), pre = spub | eph | n,
// body = AES-GCM_K(text, AAD pre), slot = AES-GCM_W(K || SHA-256(body), AAD pre).
// A slot pins its body, so a co-recipient who knows K cannot swap the body.
// kana writes one byte as one character of e2eKana, with no prefix, so the text reads
// as Japanese; the old "e2e3:" + base64 text still opens.

// e2eKana is crypto.mjs's KANA: hiragana U+3041..3096, katakana U+30A1..30FA, the 80
// first-grade kanji. Each survives Unicode NFC/NFKC, so Slack cannot rewrite it.
var e2eKana = func() []rune {
	var k []rune
	for r := rune(0x3041); r <= 0x3096; r++ {
		k = append(k, r)
	}
	for r := rune(0x30a1); r <= 0x30fa; r++ {
		k = append(k, r)
	}
	return append(k, []rune("一右雨円王音下火花貝学気九休玉金空月犬見五口校左三山子四糸字耳七車手十出女小上森人水正生青夕石赤千川先早草足村大男竹中虫町天田土二日入年白八百文木本名目立力林六")...)
}()

var e2eKanaIdx = func() map[rune]byte {
	m := make(map[rune]byte, len(e2eKana))
	for i, r := range e2eKana {
		m[r] = byte(i)
	}
	return m
}()

const e2eMin = 1 + 16 + 4 + 16 // the shortest sealed text: a pair message from POWER with no text

// e2eOver is what e2eSeal adds to the text for POWER's two recipients (the owner and itself):
// pre, a slot each (iv | K | SHA-256(body) | tag), the body's iv and tag.
const e2eOver = 32 + 32 + 1 + 2*(12+32+sha256.Size+16) + 12 + 16

// sealedMax is the most plaintext bytes a message edited in a sealed thread of form can hold.
// chat.update refuses text over about 4000 bytes (msg_too_long; the longest a card got through
// was 1332 kana) and blocks over 3000 characters (invalid_blocks). Kana is 3 bytes a byte;
// the old form is base64 behind its prefix.
func sealedMax(form string) int {
	const update, block = 1332 * 3, 3000
	switch form {
	case "pair":
		return update/3 - e2eMin
	case "kana":
		return update/3 - e2eOver
	}
	return (block-len(e2ePrefix))/4*3 - e2eOver
}

func toKana(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = e2eKana[c]
	}
	return string(r)
}

// e2eDecode is the bytes of a sealed text, either form; ok is false for any other text.
func e2eDecode(msg string) (_ []byte, ok bool) {
	s := strings.Join(strings.Fields(msg), "")
	if enc, ok := strings.CutPrefix(s, e2ePrefix); ok {
		b, err := base64.StdEncoding.DecodeString(enc)
		return b, err == nil
	}
	b := make([]byte, 0, len(s)/3)
	for _, r := range s {
		c, ok := e2eKanaIdx[r]
		if !ok {
			return nil, false
		}
		b = append(b, c)
	}
	return b, len(b) >= e2eMin
}

func wrapKey(ikm, spub, eph, rpub []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, ikm, concat(spub, eph, rpub), "e2e3 wrap", 32)
}

func concat(bs ...[]byte) []byte { return bytes.Join(bs, nil) }

func gcmSeal(key, plain, aad []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, 12)
	rand.Read(iv)
	return g.Seal(iv, iv, plain, aad), nil
}

func gcmOpen(key, buf, aad []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	if len(buf) < 12+16 {
		return nil, errors.New("e2e3: short")
	}
	return g.Open(nil, buf[:12], buf[12:], aad)
}

// e2eSeal encrypts text from sender to every recipient.
func e2eSeal(text string, sender *ecdh.PrivateKey, recipients ...*ecdh.PublicKey) (string, error) {
	if len(recipients) < 1 || len(recipients) > 255 {
		return "", errors.New("e2e3: 1..255 recipients")
	}
	k := make([]byte, 32)
	rand.Read(k)
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	spub, epub := sender.PublicKey().Bytes(), eph.PublicKey().Bytes()
	pre := concat(spub, epub, []byte{byte(len(recipients))})
	body, err := gcmSeal(k, []byte(text), pre)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(body)
	out := pre
	for _, r := range recipients {
		d1, err := eph.ECDH(r)
		if err != nil {
			return "", err
		}
		d2, err := sender.ECDH(r)
		if err != nil {
			return "", err
		}
		wk, err := wrapKey(concat(d1, d2), spub, epub, r.Bytes())
		if err != nil {
			return "", err
		}
		slot, err := gcmSeal(wk, concat(k, h[:]), pre)
		if err != nil {
			return "", err
		}
		out = concat(out, slot)
	}
	return toKana(concat(out, body)), nil
}

// --- pair format (e2e4, see crypto.mjs) --------------------------------------
//
// b0 | nonce(16) | AES-256-GCM_K(time(4) | text, iv 0, AAD b0|nonce) | tag [| self(16)], b0 0xE1 from the
// owner (who appends a self tag only the owner can check), 0xE2 from POWER. K = HKDF(X25519(owner, POWER),
// salt nonce, info "e2e4 pair"|b0|ownerPub|powerPub). POWER is always the peer side here.

const (
	pairOwner = 0xe1
	pairPeer  = 0xe2
)

func pairKey(me *ecdh.PrivateKey, other *ecdh.PublicKey, b0 byte, nonce, ownerRaw, peerRaw []byte) ([]byte, error) {
	sh, err := me.ECDH(other)
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, sh, nonce, string(concat([]byte("e2e4 pair"), []byte{b0}, ownerRaw, peerRaw)), 32)
}

func pairGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// e2ePairSeal is POWER's text to the owner, in kana.
func e2ePairSeal(text string, me *ecdh.PrivateKey, owner *ecdh.PublicKey, now time.Time) (string, error) {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	head := concat([]byte{pairPeer}, nonce)
	k, err := pairKey(me, owner, pairPeer, nonce, owner.Bytes(), me.PublicKey().Bytes())
	if err != nil {
		return "", err
	}
	g, err := pairGCM(k)
	if err != nil {
		return "", err
	}
	t := make([]byte, 4)
	binary.BigEndian.PutUint32(t, uint32(now.Unix()))
	return toKana(g.Seal(head, make([]byte, 12), concat(t, []byte(text)), head)), nil
}

// e2ePairOpen opens a pair message to or from the owner. The owner's self tag is
// not POWER's to check: it only lets the owner's Mac tell its own messages apart.
func e2ePairOpen(b []byte, me *ecdh.PrivateKey, owner *ecdh.PublicKey) (text string, fromOwner bool, at time.Time, err error) {
	fromOwner = b[0] == pairOwner
	end := len(b)
	if fromOwner {
		end -= 16
	}
	if end < e2eMin {
		return "", false, at, errors.New("e2e4: short")
	}
	k, err := pairKey(me, owner, b[0], b[1:17], owner.Bytes(), me.PublicKey().Bytes())
	if err != nil {
		return "", false, at, err
	}
	g, err := pairGCM(k)
	if err != nil {
		return "", false, at, err
	}
	pt, err := g.Open(nil, make([]byte, 12), b[17:end], b[:17])
	if err != nil {
		return "", false, at, errNotForUs
	}
	return string(pt[4:]), fromOwner, time.Unix(int64(binary.BigEndian.Uint32(pt)), 0), nil
}

var errNotForUs = errors.New("e2e3: not addressed to us or not from the expected key")

// e2eOpen decrypts msg when it is addressed to me and was sealed by one of from.
func e2eOpen(msg string, me *ecdh.PrivateKey, from ...*ecdh.PublicKey) (string, error) {
	buf, ok := e2eDecode(msg)
	if !ok {
		return "", errors.New("e2e3: not sealed text")
	}
	if len(buf) < 65 {
		return "", errors.New("e2e3: short")
	}
	pre, spub, epub, n := buf[:65], buf[:32], buf[32:64], int(buf[64])
	bodyAt := 65 + n*e2eSlot
	if n == 0 || len(buf) < bodyAt+28 {
		return "", errors.New("e2e3: short")
	}
	var sender *ecdh.PublicKey
	for _, t := range from {
		if bytes.Equal(t.Bytes(), spub) {
			sender = t
		}
	}
	if sender == nil {
		return "", errNotForUs
	}
	ep, err := ecdh.X25519().NewPublicKey(epub)
	if err != nil {
		return "", err
	}
	d1, err := me.ECDH(ep)
	if err != nil {
		return "", err
	}
	d2, err := me.ECDH(sender)
	if err != nil {
		return "", err
	}
	wk, err := wrapKey(concat(d1, d2), spub, epub, me.PublicKey().Bytes())
	if err != nil {
		return "", err
	}
	body := buf[bodyAt:]
	h := sha256.Sum256(body)
	for i := range n {
		pin, err := gcmOpen(wk, buf[65+i*e2eSlot:65+(i+1)*e2eSlot], pre)
		if err != nil {
			continue // another recipient's slot
		}
		if len(pin) != 64 || subtle.ConstantTimeCompare(pin[32:], h[:]) != 1 {
			return "", errors.New("e2e3: body swapped")
		}
		plain, err := gcmOpen(pin[:32], body, pre)
		if err != nil {
			return "", fmt.Errorf("e2e3: body: %v", err)
		}
		return string(plain), nil
	}
	return "", errNotForUs
}

// --- adapter -----------------------------------------------------------------

var threadTS = regexp.MustCompile(`^[0-9]{1,20}\.[0-9]{1,10}$`)

// A sealed owner-DM thread is one empty file <data>/e2e-sealed/<root ts>: serve and
// `power dm` (a script step) both add to it, so neither rewrites the other's state file.
func (a *adapter) sealedPath(ts string) string { return filepath.Join(a.dir, "e2e-sealed", ts) }

// isSealed reports whether thread ts is sealed. ErrNotExist means not sealed;
// any other stat error (permission, IO) is logged and returned so callers can
// decide instead of failing closed.
func (a *adapter) isSealed(ts string) (bool, error) {
	if !threadTS.MatchString(ts) {
		return false, nil
	}
	_, err := os.Stat(a.sealedPath(ts))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		a.logErr(fmt.Errorf("e2e: stat sealed thread %s: %w", ts, err))
		return false, err
	}
	return true, nil
}

func (a *adapter) markSealed(ts, form string) error {
	if !threadTS.MatchString(ts) {
		return fmt.Errorf("e2e: bad thread ts %q", ts)
	}
	if err := os.MkdirAll(filepath.Dir(a.sealedPath(ts)), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(a.sealedPath(ts)); err == nil {
		return nil // a thread keeps the form it was sealed in
	}
	return os.WriteFile(a.sealedPath(ts), []byte(form), 0o600)
}

// sealedForm is the form a sealed thread speaks: "pair", "kana" (e2e3 bytes as kana) or
// "" for the "e2e3:" + base64 of marks from before kana, so a conversation never changes form halfway.
func (a *adapter) sealedForm(ts string) string {
	b, _ := os.ReadFile(a.sealedPath(ts))
	return string(b)
}

// asLegacy rewrites a kana ciphertext in the old "e2e3:" + base64 form: same bytes.
func asLegacy(ct string) string {
	b, _ := e2eDecode(ct)
	return e2ePrefix + base64.StdEncoding.EncodeToString(b)
}

// sealedButton is the plaintext of a sealed button value.
type sealedButton struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// buttonValue is the option value inside an opened button value. Values sealed
// before labels rode along are the bare option value.
// ponytail: an option value that itself starts with {"label": would be misread; none does
func buttonValue(plain string) string {
	var b sealedButton
	if strings.HasPrefix(plain, `{"label":`) && json.Unmarshal([]byte(plain), &b) == nil {
		return b.Value
	}
	return plain
}

func sealedText(s string) bool { _, ok := e2eDecode(s); return ok }

// unseal is the plaintext of a message sealed by the key of who ("owner" or
// "bot", POWER herself); any other text is returned as it is. ok is false for
// e2e3 text POWER cannot read or that another key sealed: POWER's own sealed
// reply posted under the owner's name is not the owner speaking.
func (a *adapter) unseal(text, who string) (string, bool) {
	plain, _, _, ok := a.unsealForm(text, who)
	return plain, ok
}

// unsealForm is unseal plus the form text was sealed in and, for a pair message, when it was sealed.
func (a *adapter) unsealForm(text, who string) (plain, form string, sealedAt time.Time, ok bool) {
	b, sealed := e2eDecode(text)
	if !sealed {
		return text, "", time.Time{}, true
	}
	if a.e2e == nil {
		return text, "", time.Time{}, false
	}
	form = "kana"
	if strings.HasPrefix(strings.TrimSpace(text), e2ePrefix) {
		form = ""
	} else if b[0] == pairOwner || b[0] == pairPeer {
		if pt, fromOwner, at, err := e2ePairOpen(b, a.e2e.me, a.e2e.owner); err == nil {
			return pt, "pair", at, fromOwner == (who == "owner")
		}
		// else an e2e3 header whose first byte looks like a pair one: tried below
	}
	from := a.e2e.me.PublicKey()
	if who == "owner" {
		from = a.e2e.owner
	}
	pt, err := e2eOpen(text, a.e2e.me, from)
	if err != nil {
		return text, form, time.Time{}, false
	}
	return pt, form, time.Time{}, true
}

// sealOut is what goes to Slack for a post or a redraw: in a sealed thread of
// the owner DM, the text and every section and context text are ciphertext;
// buttons become "Option n" with a sealed value, explained by a sealed legend;
// any other block or element (accessories, selects, headers) is dropped, never sent.
// run is the run the post speaks for (a contract's or milestone's; empty: $KAMI_RUN of a script step).
// newRoot is a sealed top-level post: the thread under it must be marked sealed once posted.
func (a *adapter) sealOut(ctx context.Context, channel, thread, run, text string, blocks []slack.Block) (_ string, _ []slack.Block, newRoot bool, _ error) {
	if a.e2e == nil {
		return text, blocks, false, nil
	}
	if run == "" {
		run = a.run
	}
	key, kek, sealedKek := thread, "", false
	if run != "" && (key == "" || len(a.e2e.kekkai) > 0) {
		// a script step posting a new root (launch's 🧵 thread for "thread me ..."): sealed when the
		// conversation its run speaks for is, or the request would leave in plaintext in a new thread
		l, err := a.runLink(run, 0)
		if err != nil {
			return "", nil, false, fmt.Errorf("e2e: thread of run %s: %w", run, err)
		}
		if key == "" {
			key = l.thread
		}
		kek, sealedKek = l.kekkai, a.e2e.kekkai[l.kekkai]
	}
	fresh := false // a sealed kekkai's run with no thread yet: this post opens a sealed one
	sealed := false
	if key == "" {
		// no thread yet: sealed only if the kekkai itself demands it
	} else if s, err := a.isSealed(key); err != nil {
		return "", nil, false, err
	} else {
		sealed = s
	}
	if key == "" || !sealed {
		switch {
		case !sealedKek:
			return text, blocks, false, nil
		case key != "":
			return "", nil, false, fmt.Errorf("e2e: run %s of sealed kekkai %s posts into plaintext thread %s: refused", run, kek, key)
		}
		fresh = true
	}
	if im, err := a.dm(ctx); err != nil {
		return "", nil, false, err
	} else if channel != "" && channel != im {
		if sealedKek {
			return "", nil, false, fmt.Errorf("e2e: run %s of sealed kekkai %s posts to channel %s: only the owner DM is sealed, refused", run, kek, channel)
		}
		return text, blocks, false, nil // the key is the owner DM's thread ts; another channel's thread is not sealed
	}
	form := "pair"
	if !fresh {
		form = a.sealedForm(key)
	}
	seal := func(s string) (string, error) {
		if strings.TrimSpace(s) == "" {
			return s, nil
		}
		// always sealed, even text that looks like e2e3 already: plaintext starting "e2e3:" must not pass
		if form == "pair" {
			return e2ePairSeal(s, a.e2e.me, a.e2e.owner, time.Now())
		}
		ct, err := e2eSeal(s, a.e2e.me, a.e2e.owner, a.e2e.me.PublicKey())
		if err == nil && form == "" {
			ct = asLegacy(ct)
		}
		return ct, err
	}
	st, err := seal(text)
	if err != nil {
		return "", nil, false, err
	}
	out := make([]slack.Block, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case *slack.SectionBlock:
			// sent as rich_text, not a section: Slack's client cuts long section text to "…" + Show more,
			// and a cut ciphertext cannot be opened. Fields and accessory are dropped rather than leaked.
			if b.Text == nil {
				continue
			}
			s, err := seal(b.Text.Text)
			if err != nil {
				return "", nil, false, err
			}
			out = append(out, slack.NewRichTextBlock(b.BlockID, slack.NewRichTextSection(slack.NewRichTextSectionTextElement(s, nil))))
		case *slack.ActionBlock:
			var els []slack.BlockElement
			var legend []string
			for _, el := range b.Elements.ElementSet {
				btn, ok := el.(*slack.ButtonBlockElement)
				if !ok {
					continue // a select or picker would show its options: dropped
				}
				n := len(els) + 1
				label := btn.Value
				if btn.Text != nil {
					label = btn.Text.Text
				}
				// the label rides inside the sealed value, so sshhh can draw it on this very button:
				// a label read from the legend could be put on any "Option n" button
				pv, _ := json.Marshal(sealedButton{Label: label, Value: btn.Value})
				v, err := seal(string(pv))
				if err != nil {
					return "", nil, false, err
				}
				nb := slack.NewButtonBlockElement(btn.ActionID, v, slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Option %d", n), false, false))
				nb.Style = btn.Style
				els = append(els, nb)
				legend = append(legend, fmt.Sprintf("Option %d: %s", n, label))
			}
			if len(els) == 0 {
				continue
			}
			l, err := seal(strings.Join(legend, "\n"))
			if err != nil {
				return "", nil, false, err
			}
			out = append(out, slack.NewActionBlock(b.BlockID, els...),
				slack.NewContextBlock("", slack.NewTextBlockObject(slack.PlainTextType, l, false, false)))
		case *slack.DividerBlock:
			out = append(out, b)
		case *slack.ContextBlock:
			var els []slack.MixedElement
			for _, el := range b.ContextElements.Elements {
				if t, ok := el.(*slack.TextBlockObject); ok {
					s, err := seal(t.Text)
					if err != nil {
						return "", nil, false, err
					}
					els = append(els, slack.NewTextBlockObject(slack.PlainTextType, s, false, false))
				}
			}
			if len(els) > 0 {
				out = append(out, slack.NewContextBlock(b.BlockID, els...))
			}
		}
	}
	if blocks == nil {
		out = nil
	}
	return st, out, thread == "", nil
}

// newRootForm is the form a sealed new root (sealOut's newRoot) is marked with: its run's thread's,
// or pair when the run has no sealed thread yet (a sealed kekkai's first post).
func (a *adapter) newRootForm(thread, run string) string {
	if run == "" {
		run = a.run
	}
	if thread == "" && run != "" {
		if l, err := a.runLink(run, 0); err == nil && l.thread != "" {
			if sealed, e2 := a.isSealed(l.thread); e2 == nil && sealed {
				return a.sealedForm(l.thread)
			}
		}
	}
	return "pair"
}
