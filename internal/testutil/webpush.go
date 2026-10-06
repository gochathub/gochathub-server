// Package testutil holds test-only helpers (receiver-side crypto the
// production binary never links).
package testutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// ReceiverKey returns an ECDH P-256 private key plus the 65-byte SEC1
// uncompressed public key connectors hand out as `p256dh`, and a random
// 16-byte auth secret.
func ReceiverKey() (priv *ecdh.PrivateKey, p256dh []byte, auth []byte, err error) {
	priv, err = ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	auth = make([]byte, 16)
	if _, err = rand.Read(auth); err != nil {
		return nil, nil, nil, err
	}
	return priv, priv.PublicKey().Bytes(), auth, nil
}

func hkdfOut(ikm, salt, info []byte, outLen int) []byte {
	r := hkdf.New(sha256.New, ikm, salt, info)
	out := make([]byte, outLen)
	if _, err := io.ReadFull(r, out); err != nil {
		panic(err)
	}
	return out
}

// DecryptRFC8291 undoes the aes128gcm binary layout (RFC 8188 §2.2) with the
// Web Push key schedule (RFC 8291 §4) — the mirror of webpush-go's sender
// side. Test verification only; production never decrypts pushes.
func DecryptRFC8291(body []byte, priv *ecdh.PrivateKey, receiverPub, authSecret []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, fmt.Errorf("body too short")
	}
	salt := body[:16]
	rs := int(body[16])<<24 | int(body[17])<<16 | int(body[18])<<8 | int(body[19])
	idlen := int(body[20])
	if idlen == 0 || 21+idlen > len(body) {
		return nil, fmt.Errorf("bad keyid length %d", idlen)
	}
	senderPubRaw := body[21 : 21+idlen]
	records := body[21+idlen:]
	if rs == 0 || len(records) == 0 {
		return nil, fmt.Errorf("empty payload")
	}

	senderPub, err := ecdh.P256().NewPublicKey(senderPubRaw)
	if err != nil {
		return nil, fmt.Errorf("sender key invalid: %w", err)
	}
	shared, err := priv.ECDH(senderPub)
	if err != nil {
		return nil, fmt.Errorf("ecdh: %w", err)
	}
	info := append([]byte("WebPush: info\x00"), receiverPub...)
	info = append(info, senderPubRaw...)
	ikm := hkdfOut(shared, authSecret, info, 32)
	cek := hkdfOut(ikm, salt, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfOut(ikm, salt, []byte("Content-Encoding: nonce\x00"), 12)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	var out []byte
	seq := make([]byte, 12) // record sequence number, right-aligned
	for len(records) > 0 {
		size := rs
		if len(records) < rs {
			size = len(records)
		}
		rec := records[:size]
		records = records[size:]
		if len(rec) <= gcm.Overhead() {
			return nil, fmt.Errorf("record too short")
		}
		iv := make([]byte, len(nonce))
		for i := range iv {
			iv[i] = nonce[i] ^ seq[i]
		}
		plain, err := gcm.Open(nil, iv, rec, nil)
		if err != nil {
			return nil, fmt.Errorf("record open: %w", err)
		}
		out = append(out, plain...)
		for i := len(seq) - 1; i >= 0; i-- {
			seq[i]++
			if seq[i] != 0 {
				break
			}
		}
	}
	cut := -1
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] == 0x02 {
			cut = i
			break
		}
		if out[i] != 0x00 {
			break
		}
	}
	if cut < 0 {
		return nil, fmt.Errorf("padding delimiter missing")
	}
	return out[:cut], nil
}
