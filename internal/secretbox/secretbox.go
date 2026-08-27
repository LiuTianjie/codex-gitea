// Package secretbox encrypts console-managed integration credentials before
// they are persisted in SQLite.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const prefix = "enc:v1:"

var ErrKeyRequired = errors.New("SECRET_KEY is required to store integration credentials")

type Box struct {
	aead cipher.AEAD
}

func New(secret string) (*Box, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, ErrKeyRequired
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("create secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create secret gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if b == nil || b.aead == nil {
		return "", ErrKeyRequired
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate secret nonce: %w", err)
	}
	sealed := b.aead.Seal(nil, nonce, []byte(value), nil)
	payload := append(nonce, sealed...)
	return prefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (b *Box) Open(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !strings.HasPrefix(value, prefix) {
		return "", errors.New("secret value is not encrypted")
	}
	if b == nil || b.aead == nil {
		return "", ErrKeyRequired
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("decode secret value: %w", err)
	}
	if len(payload) < b.aead.NonceSize() {
		return "", errors.New("encrypted secret is truncated")
	}
	nonce, ciphertext := payload[:b.aead.NonceSize()], payload[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret value: %w", err)
	}
	return string(plain), nil
}
