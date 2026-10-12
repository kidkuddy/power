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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
}

type sealer struct {
	me    *ecdh.PrivateKey
	owner *ecdh.PublicKey
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
	return &sealer{me, owner}, nil
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
// "e2e3:" + base64( pre | n × slot | body ), pre = spub | eph | n,
// body = AES-GCM_K(text, AAD pre), slot = AES-GCM_W(K || SHA-256(body), AAD pre).
// A slot pins its body, so a co-recipient who knows K cannot swap the body.

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
	return e2ePrefix + base64.StdEncoding.EncodeToString(concat(out, body)), nil
}

var errNotForUs = errors.New("e2e3: not addressed to us or not from the expected key")

// e2eOpen decrypts msg when it is addressed to me and was sealed by one of from.
func e2eOpen(msg string, me *ecdh.PrivateKey, from ...*ecdh.PublicKey) (string, error) {
	enc, ok := strings.CutPrefix(strings.TrimSpace(msg), e2ePrefix)
	if !ok {
		return "", errors.New("e2e3: no prefix")
	}
	buf, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(enc), ""))
	if err != nil {
		return "", fmt.Errorf("e2e3: %v", err)
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

// isSealed fails closed: a mark it cannot check counts as sealed.
func (a *adapter) isSealed(ts string) bool {
	if !threadTS.MatchString(ts) {
		return false
	}
	_, err := os.Stat(a.sealedPath(ts))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func (a *adapter) markSealed(ts string) error {
	if !threadTS.MatchString(ts) {
		return fmt.Errorf("e2e: bad thread ts %q", ts)
	}
	if err := os.MkdirAll(filepath.Dir(a.sealedPath(ts)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.sealedPath(ts), nil, 0o600)
}

func sealedText(s string) bool { return strings.HasPrefix(strings.TrimSpace(s), e2ePrefix) }

// unseal is the plaintext of a message sealed by the key of who ("owner" or
// "bot", POWER herself); any other text is returned as it is. ok is false for
// e2e3 text POWER cannot read or that another key sealed: POWER's own sealed
// reply posted under the owner's name is not the owner speaking.
func (a *adapter) unseal(text, who string) (string, bool) {
	if !sealedText(text) {
		return text, true
	}
	if a.e2e == nil {
		return text, false
	}
	from := a.e2e.me.PublicKey()
	if who == "owner" {
		from = a.e2e.owner
	}
	plain, err := e2eOpen(text, a.e2e.me, from)
	if err != nil {
		return text, false
	}
	return plain, true
}

// sealOut is what goes to Slack for a post or a redraw: in a sealed thread of
// the owner DM, the text and every section and context text are ciphertext;
// buttons become "Option n" with a sealed value, explained by a sealed legend;
// any other block or element (accessories, selects, headers) is dropped, never sent.
// newRoot is a sealed top-level post: the thread under it must be marked sealed once posted.
func (a *adapter) sealOut(ctx context.Context, channel, thread, text string, blocks []slack.Block) (_ string, _ []slack.Block, newRoot bool, _ error) {
	if a.e2e == nil {
		return text, blocks, false, nil
	}
	key := thread
	if key == "" && a.run != "" {
		// a script step posting a new root (launch's 🧵 thread for "thread me ..."): sealed when the
		// conversation its run speaks for is, or the request would leave in plaintext in a new thread
		l, err := a.runLink(a.run, 0)
		if err != nil {
			return "", nil, false, fmt.Errorf("e2e: thread of run %s: %w", a.run, err)
		}
		key = l.thread
	}
	if key == "" || !a.isSealed(key) {
		return text, blocks, false, nil
	}
	if im, err := a.dm(ctx); err != nil {
		return "", nil, false, err
	} else if channel != "" && channel != im {
		return text, blocks, false, nil // the key is the owner DM's thread ts; another channel's thread is not sealed
	}
	seal := func(s string) (string, error) {
		if strings.TrimSpace(s) == "" {
			return s, nil
		}
		// always sealed, even text that looks like e2e3 already: plaintext starting "e2e3:" must not pass
		return e2eSeal(s, a.e2e.me, a.e2e.owner, a.e2e.me.PublicKey())
	}
	st, err := seal(text)
	if err != nil {
		return "", nil, false, err
	}
	out := make([]slack.Block, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case *slack.SectionBlock:
			c := *b
			if c.Text != nil {
				t := *c.Text
				if t.Text, err = seal(t.Text); err != nil {
					return "", nil, false, err
				}
				t.Type = slack.PlainTextType // the ciphertext is not mrkdwn
				c.Text = &t
			}
			c.Fields, c.Accessory = nil, nil // never used here; dropped rather than leaked
			out = append(out, &c)
		case *slack.ActionBlock:
			var els []slack.BlockElement
			var legend []string
			for _, el := range b.Elements.ElementSet {
				btn, ok := el.(*slack.ButtonBlockElement)
				if !ok {
					continue // a select or picker would show its options: dropped
				}
				n := len(els) + 1
				v, err := seal(btn.Value)
				if err != nil {
					return "", nil, false, err
				}
				nb := slack.NewButtonBlockElement(btn.ActionID, v, slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Option %d", n), false, false))
				nb.Style = btn.Style
				els = append(els, nb)
				label := btn.Value
				if btn.Text != nil {
					label = btn.Text.Text
				}
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
