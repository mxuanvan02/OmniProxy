package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// encPrefix marks an encrypted-at-rest value in the config file. Values
// without the prefix are treated as legacy plaintext and pass through
// unchanged; they are encrypted on the next save.
const encPrefix = "enc:v1:"

// keyFileName is the encryption key file, stored next to the config file with
// 0600 permissions. Losing it means losing every stored credential, so it is
// never rotated implicitly and never written anywhere else.
const keyFileName = "config.key"

var (
	keyMu      sync.Mutex
	cachedKeys = map[string][]byte{} // dir -> key
)

// encryptionKeyFor returns the 32-byte AES key for the config directory,
// creating the key file on first use. The key is cached per directory because
// tests (and multi-config setups) re-Init with different paths.
func encryptionKeyFor(dir string) ([]byte, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	if key, ok := cachedKeys[dir]; ok {
		return key, nil
	}
	keyPath := filepath.Join(dir, keyFileName)
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate config key: %w", err)
		}
		tmp, err := os.CreateTemp(dir, "."+keyFileName+".tmp-*")
		if err != nil {
			return nil, err
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return nil, err
		}
		if _, err := tmp.Write(key); err != nil {
			tmp.Close()
			return nil, err
		}
		if err := tmp.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(tmpPath, keyPath); err != nil {
			return nil, err
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("config key file %s is %d bytes, want 32", keyPath, len(key))
	}
	cachedKeys[dir] = key
	return key, nil
}

func encryptValue(plaintext string, dir string) (string, error) {
	if plaintext == "" || strings.HasPrefix(plaintext, encPrefix) {
		return plaintext, nil
	}
	key, err := encryptionKeyFor(dir)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func decryptValue(value string, dir string) (string, error) {
	if !strings.HasPrefix(value, encPrefix) {
		return value, nil
	}
	key, err := encryptionKeyFor(dir)
	if err != nil {
		return "", err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, encPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("encrypted config value too short")
	}
	plaintext, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt config value: %w", err)
	}
	return string(plaintext), nil
}

// encryptSensitiveFields walks the marshalled config JSON and replaces the
// credential fields with encrypted forms. It operates on the generic JSON tree
// so new credential-bearing structs are covered without a field registry here.
func encryptSensitiveFields(data []byte, dir string) ([]byte, error) {
	var tree map[string]json.RawMessage
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	changed := false

	if raw, ok := tree["accounts"]; ok {
		var accounts []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &accounts); err == nil {
			for _, acc := range accounts {
				for _, field := range []string{"accessToken", "refreshToken", "clientSecret", "extAdminToken"} {
					enc, err := encryptRawField(acc[field], dir)
					if err != nil {
						return nil, err
					}
					if enc != nil {
						acc[field] = enc
						changed = true
					}
				}
			}
			if changed {
				merged, err := json.Marshal(accounts)
				if err != nil {
					return nil, err
				}
				tree["accounts"] = merged
			}
		}
	}

	if !changed {
		return data, nil
	}
	return json.MarshalIndent(tree, "", "  ")
}

// encryptRawField encrypts one JSON string value. nil means "leave unchanged":
// missing, empty, or already-encrypted values pass through.
func encryptRawField(raw json.RawMessage, dir string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, nil // not a string; leave untouched
	}
	enc, err := encryptValue(value, dir)
	if err != nil {
		return nil, err
	}
	if enc == value {
		return nil, nil
	}
	return json.Marshal(enc)
}

// decryptAccountCredentials restores plaintext credential values in memory
// after a load. Legacy plaintext values pass through unchanged.
func decryptAccountCredentials() {
	if cfg == nil || cfgPath == "" {
		return
	}
	dir := filepath.Dir(cfgPath)
	decrypt := func(value string) string {
		if !strings.HasPrefix(value, encPrefix) {
			return value
		}
		plaintext, err := decryptValue(value, dir)
		if err != nil {
			// A value that cannot be decrypted is unusable either way; keep
			// the stored form so the operator sees the broken state instead
			// of an empty token that looks healthy.
			return value
		}
		return plaintext
	}
	for i := range cfg.Accounts {
		cfg.Accounts[i].AccessToken = decrypt(cfg.Accounts[i].AccessToken)
		cfg.Accounts[i].RefreshToken = decrypt(cfg.Accounts[i].RefreshToken)
		cfg.Accounts[i].ClientSecret = decrypt(cfg.Accounts[i].ClientSecret)
		cfg.Accounts[i].ExtAdminToken = decrypt(cfg.Accounts[i].ExtAdminToken)
	}
}
