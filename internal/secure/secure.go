package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const credentialAAD = "icloud-api/imap-credential/v1"

type Cipher struct {
	aead cipher.AEAD
}

func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥必须是 32 字节")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成随机数: %w", err)
	}
	ciphertext := c.aead.Seal(nil, nonce, []byte(plaintext), []byte(credentialAAD))
	payload := append(nonce, ciphertext...)
	return "v1." + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (c *Cipher) Decrypt(value string) (string, error) {
	version, encoded, ok := strings.Cut(value, ".")
	if !ok || version != "v1" {
		return "", errors.New("未知的凭据密文版本")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("凭据密文格式错误")
	}
	if len(payload) < c.aead.NonceSize()+c.aead.Overhead() {
		return "", errors.New("凭据密文长度错误")
	}
	nonce := payload[:c.aead.NonceSize()]
	ciphertext := payload[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(credentialAAD))
	if err != nil {
		return "", errors.New("凭据解密失败，请检查主密钥")
	}
	return string(plaintext), nil
}

func LoadOrCreateMasterKey(path string) ([]byte, bool, error) {
	if value := strings.TrimSpace(os.Getenv("ICLOUD_API_MASTER_KEY")); value != "" {
		key, err := decodeKey(value)
		return key, false, err
	}
	if data, err := os.ReadFile(path); err == nil {
		key, decodeErr := decodeKey(strings.TrimSpace(string(data)))
		return key, false, decodeErr
	} else if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("读取主密钥文件: %w", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("生成主密钥: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("创建主密钥目录: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, false, fmt.Errorf("写入主密钥文件: %w", err)
	}
	return key, true, nil
}

func decodeKey(value string) ([]byte, error) {
	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.RawURLEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		if key, err := decode(value); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, errors.New("ICLOUD_API_MASTER_KEY 必须是 32 字节的 Base64 或十六进制值")
}

func RandomToken(byteLength int) (string, error) {
	value := make([]byte, byteLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func NewAPIKey() (raw string, hash []byte, prefix string, err error) {
	secret, err := RandomToken(32)
	if err != nil {
		return "", nil, "", err
	}
	raw = "icm_" + secret
	hash = HashToken(raw)
	prefix = raw[:12]
	return raw, hash, prefix, nil
}

func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func HashEqual(left, right []byte) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1
}

func RandomPassword() (string, error) {
	value, err := RandomToken(18)
	if err != nil {
		return "", err
	}
	return value, nil
}
