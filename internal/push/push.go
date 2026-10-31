// Package push is how a phone hears about held messages without staying
// on the mesh. The phone's push token is in peers.json (identity.Phone),
// synced to all your machines; a session that holds a message, or sees one
// dealt with, seals a Note with a key derived from your self secret and
// asks the rendezvous server to forward it through Firebase Cloud
// Messaging. Only your devices hold the
// self secret, so the server and Google see ciphertext and a token.
package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/vitaminmoo/peerbus/internal/identity"
)

// Note kinds.
const (
	Held = "held" // a message is waiting: show it
	Gone = "gone" // it was approved, routed or declined somewhere: clear it
)

// maxText keeps a sealed note well inside FCM's 4 KB payload.
const maxText = 1500

// Note is what a phone shows, or clears.
type Note struct {
	Kind  string `json:"k"`
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Text  string `json:"text,omitempty"`
}

func key(secret identity.Secret) []byte {
	m := hmac.New(sha256.New, secret[:])
	m.Write([]byte("peerbus push v1"))
	return m.Sum(nil)
}

func aead(secret identity.Secret) cipher.AEAD {
	b, _ := aes.NewCipher(key(secret))
	g, _ := cipher.NewGCM(b)
	return g
}

// Seal encrypts n for your devices: base64 of nonce and AES-256-GCM
// ciphertext.
func Seal(secret identity.Secret, n Note) string {
	n.Text = clip(n.Text, maxText)
	n.Title = clip(n.Title, 200)
	plain, _ := json.Marshal(n)
	g := aead(secret)
	nonce := make([]byte, g.NonceSize(), g.NonceSize()+len(plain)+g.Overhead())
	rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(g.Seal(nonce, nonce, plain, nil))
}

// Open decrypts a sealed note.
func Open(secret identity.Secret, sealed string) (Note, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	g := aead(secret)
	if err != nil || len(b) < g.NonceSize() {
		return Note{}, errors.New("not a sealed note")
	}
	plain, err := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], nil)
	if err != nil {
		return Note{}, errors.New("note isn't for this identity")
	}
	var n Note
	if err := json.Unmarshal(plain, &n); err != nil {
		return Note{}, err
	}
	return n, nil
}

// clip shortens s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return strings.TrimRight(s, " \n") + "…"
}
