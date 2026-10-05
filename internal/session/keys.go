package session

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of a session key: 32 random bytes, written in standard
// base64, one key per line. `head -c 32 /dev/urandom | base64` makes one.
const KeySize = 32

// MaxKeys bounds the keys a deployment configures: the one that seals new cookies and
// those that only still open them while a rotation is under way. Every key is tried
// by its ID, never by trial decryption, but an unbounded list would be a list nobody
// prunes, and a key nobody prunes keeps its cookies usable.
const MaxKeys = 4

// keyIDSize is how much of a key's ID a cookie carries, so the right key opens it.
const keyIDSize = 8

// Keys seal and open session cookies. The first key seals every new cookie; every key
// opens the cookies sealed with it. Only krm-foyer holds them: whoever has a key can
// read the tokens in every cookie it sealed, and make cookies of their own.
type Keys struct {
	keys []key
}

type key struct {
	id   [keyIDSize]byte
	aead cipher.AEAD
}

// ParseKeys reads keys from text: one per line, in standard base64, each KeySize
// bytes, the first the one that seals. Blank lines are ignored; anything else that is
// not a key is an error, as is no key, a repeated key, or more than MaxKeys. A bad
// key never falls back to another: krm-foyer does not start without the keys it was
// given.
func ParseKeys(text []byte) (*Keys, error) {
	var ks Keys
	seen := map[[KeySize]byte]bool{}
	for n, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(line)
		if err != nil || len(raw) != KeySize {
			// Not the line itself: it is meant to be a secret.
			return nil, fmt.Errorf("line %d is not %d bytes in standard base64", n+1, KeySize)
		}
		secret := [KeySize]byte(raw)
		if seen[secret] {
			return nil, fmt.Errorf("line %d repeats a key", n+1)
		}
		seen[secret] = true
		k, err := newKey(raw)
		if err != nil {
			return nil, err
		}
		ks.keys = append(ks.keys, k)
	}
	switch {
	case len(ks.keys) == 0:
		return nil, errors.New("no session key")
	case len(ks.keys) > MaxKeys:
		return nil, fmt.Errorf("%d session keys, at most %d: remove the keys whose cookies have expired", len(ks.keys), MaxKeys)
	}
	return &ks, nil
}

// newKey derives the cipher and the ID of a configured key. Neither is the key
// itself: the cipher's key is derived for session cookies alone, and the ID, which
// every cookie carries in the clear, is derived apart from it.
func newKey(secret []byte) (key, error) {
	enc, err := hkdf.Key(sha256.New, secret, nil, "krm-foyer session cookie v1 encryption", 32)
	if err != nil {
		return key{}, err
	}
	id, err := hkdf.Key(sha256.New, secret, nil, "krm-foyer session cookie v1 key id", keyIDSize)
	if err != nil {
		return key{}, err
	}
	block, err := aes.NewCipher(enc)
	if err != nil {
		return key{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return key{}, err
	}
	return key{id: [keyIDSize]byte(id), aead: aead}, nil
}

// sealing is the key new cookies are sealed with.
func (ks *Keys) sealing() key { return ks.keys[0] }

// byID is the key with this ID, if one is configured.
func (ks *Keys) byID(id []byte) (key, bool) {
	for _, k := range ks.keys {
		if bytes.Equal(k.id[:], id) {
			return k, true
		}
	}
	return key{}, false
}
