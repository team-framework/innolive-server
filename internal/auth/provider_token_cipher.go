package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const providerTokenKeyVersion int16 = 1

// ProviderTokenCipher encrypts provider refresh tokens before persistence.
// Ciphertexts contain nonce || sealed data, so every stored value has its own
// AES-GCM nonce and can be decrypted after a process restart using the same key.
type ProviderTokenCipher struct {
	key []byte
}

func NewProviderTokenCipherFromBase64(value string) (*ProviderTokenCipher, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("AUTH_PROVIDER_TOKEN_ENCRYPTION_KEY_BASE64 is required when Apple OAuth is enabled")
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("AUTH_PROVIDER_TOKEN_ENCRYPTION_KEY_BASE64 is not valid base64")
	}
	if len(key) != 32 {
		return nil, errors.New("AUTH_PROVIDER_TOKEN_ENCRYPTION_KEY_BASE64 must decode to exactly 32 bytes")
	}
	return &ProviderTokenCipher{key: key}, nil
}

func (c *ProviderTokenCipher) Encrypt(plaintext string) ([]byte, *int16, error) {
	if c == nil || len(c.key) != 32 {
		return nil, nil, errors.New("provider token cipher is unavailable")
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	result := append(nonce, ciphertext...)
	version := providerTokenKeyVersion
	return result, &version, nil
}

func (c *ProviderTokenCipher) Decrypt(ciphertext []byte, version *int16) (string, error) {
	if c == nil || len(c.key) != 32 {
		return "", errors.New("provider token cipher is unavailable")
	}
	if version == nil || *version != providerTokenKeyVersion {
		return "", errors.New("unsupported provider token key version")
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create AES-GCM: %w", err)
	}
	if len(ciphertext) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("provider token ciphertext is malformed")
	}
	nonce, sealed := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", errors.New("provider token ciphertext cannot be decrypted")
	}
	return string(plaintext), nil
}
