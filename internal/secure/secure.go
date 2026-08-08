package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	credentialAAD             = "icloud-api/imap-credential/v1"
	appleSessionAAD           = "icloud-api/apple-web-session/v1"
	pendingAliasAPIKeyAAD     = "icloud-api/pending-alias-api-key/v1"
	directLinkKeyContext      = "icloud-api/direct-link/key/v1"
	directLinkTokenContext    = "icloud-api/direct-link/token/v1"
	directLinkTokenPrefix     = "icm_"
	directLinkTokenVersion    = byte(1)
	directLinkTokenHeaderSize = 12
	directLinkTokenSize       = 32
)

var directLinkTokenMagic = [3]byte{'d', 'l', 't'}

type Cipher struct {
	aead          cipher.AEAD
	directLinkKey [sha256.Size]byte
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
	directLinkKeyMAC := hmac.New(sha256.New, key)
	_, _ = directLinkKeyMAC.Write([]byte(directLinkKeyContext))
	var directLinkKey [sha256.Size]byte
	copy(directLinkKey[:], directLinkKeyMAC.Sum(nil))
	return &Cipher{aead: aead, directLinkKey: directLinkKey}, nil
}

func (c *Cipher) Encrypt(plaintext string) (string, error) {
	return c.encrypt("v1", credentialAAD, plaintext)
}

func (c *Cipher) EncryptAppleSession(plaintext string) (string, error) {
	return c.encrypt("as1", appleSessionAAD, plaintext)
}

// EncryptPendingAliasAPIKey encrypts a one-time API key that was created by a
// background job. A dedicated version and AAD prevent this ciphertext from
// being accepted as either an IMAP credential or an Apple web session.
func (c *Cipher) EncryptPendingAliasAPIKey(plaintext string) (string, error) {
	return c.encrypt("ak1", pendingAliasAPIKeyAAD, plaintext)
}

func (c *Cipher) encrypt(version, aad, plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成随机数: %w", err)
	}
	ciphertext := c.aead.Seal(nil, nonce, []byte(plaintext), []byte(aad))
	payload := append(nonce, ciphertext...)
	return version + "." + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (c *Cipher) Decrypt(value string) (string, error) {
	return c.decrypt("v1", credentialAAD, value)
}

func (c *Cipher) DecryptAppleSession(value string) (string, error) {
	return c.decrypt("as1", appleSessionAAD, value)
}

func (c *Cipher) DecryptPendingAliasAPIKey(value string) (string, error) {
	return c.decrypt("ak1", pendingAliasAPIKeyAAD, value)
}

func (c *Cipher) decrypt(expectedVersion, aad, value string) (string, error) {
	version, encoded, ok := strings.Cut(value, ".")
	if !ok || version != expectedVersion {
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
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(aad))
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
	if key, err := readMasterKey(path); err == nil {
		return key, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("生成主密钥: %w", err)
	}
	created, err := publishMasterKey(path, key)
	if err != nil {
		return nil, false, err
	}
	if !created {
		persisted, err := readMasterKey(path)
		if err != nil {
			return nil, false, err
		}
		return persisted, false, nil
	}
	return key, true, nil
}

func readMasterKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取主密钥文件: %w", err)
	}
	key, err := decodeKey(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("解析主密钥文件: %w", err)
	}
	return key, nil
}

func publishMasterKey(path string, key []byte) (bool, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, fmt.Errorf("创建主密钥目录: %w", err)
	}
	if directory != "." {
		if err := os.Chmod(directory, 0o700); err != nil {
			return false, fmt.Errorf("设置主密钥目录权限: %w", err)
		}
	}

	temporary, err := os.CreateTemp(directory, ".master-key-*.tmp")
	if err != nil {
		return false, fmt.Errorf("创建主密钥临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return false, fmt.Errorf("设置主密钥文件权限: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if _, err := temporary.WriteString(encoded); err != nil {
		temporary.Close()
		return false, fmt.Errorf("写入主密钥临时文件: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return false, fmt.Errorf("同步主密钥临时文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("关闭主密钥临时文件: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() {
			return false, nil
		}
		return false, fmt.Errorf("发布主密钥文件: %w", err)
	}
	return true, nil
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

// DirectLinkToken deterministically derives a URL credential for one alias.
// The credential embeds a versioned alias ID and authenticates the alias's
// current API key hash, so rotating that API key invalidates prior links
// without storing another secret.
func (c *Cipher) DirectLinkToken(aliasID int64, apiKeyHash []byte) (string, error) {
	if c == nil {
		return "", errors.New("直达链接签名器不可用")
	}
	if aliasID < 1 {
		return "", errors.New("隐私邮箱 ID 必须是正整数")
	}
	if len(apiKeyHash) != sha256.Size {
		return "", errors.New("API Key 哈希必须是 32 字节")
	}

	payload := make([]byte, directLinkTokenSize)
	copy(payload[:len(directLinkTokenMagic)], directLinkTokenMagic[:])
	payload[len(directLinkTokenMagic)] = directLinkTokenVersion
	binary.BigEndian.PutUint64(payload[4:directLinkTokenHeaderSize], uint64(aliasID))

	mac := hmac.New(sha256.New, c.directLinkKey[:])
	_, _ = mac.Write([]byte(directLinkTokenContext))
	_, _ = mac.Write(payload[:directLinkTokenHeaderSize])
	_, _ = mac.Write(apiKeyHash)
	copy(payload[directLinkTokenHeaderSize:], mac.Sum(nil))

	return directLinkTokenPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

// DirectLinkTokenAliasID recognizes the versioned direct-link token envelope
// and returns its untrusted alias ID. Call VerifyDirectLinkToken with the
// current stored API key hash before accepting the credential.
func DirectLinkTokenAliasID(token string) (int64, bool) {
	payload, ok := directLinkTokenPayload(token)
	if !ok {
		return 0, false
	}
	encodedID := binary.BigEndian.Uint64(payload[4:directLinkTokenHeaderSize])
	aliasID := int64(encodedID)
	if aliasID < 1 || uint64(aliasID) != encodedID {
		return 0, false
	}
	return aliasID, true
}

// VerifyDirectLinkToken authenticates a parsed token against the alias's
// current API key hash using a constant-time MAC comparison.
func (c *Cipher) VerifyDirectLinkToken(token string, aliasID int64, apiKeyHash []byte) bool {
	payload, ok := directLinkTokenPayload(token)
	if !ok || c == nil || len(apiKeyHash) != sha256.Size {
		return false
	}
	parsedID, ok := DirectLinkTokenAliasID(token)
	if !ok || parsedID != aliasID {
		return false
	}
	expected, err := c.DirectLinkToken(aliasID, apiKeyHash)
	if err != nil {
		return false
	}
	expectedPayload, ok := directLinkTokenPayload(expected)
	return ok && hmac.Equal(payload[directLinkTokenHeaderSize:], expectedPayload[directLinkTokenHeaderSize:])
}

func directLinkTokenPayload(token string) ([]byte, bool) {
	if len(token) != len(directLinkTokenPrefix)+43 || !strings.HasPrefix(token, directLinkTokenPrefix) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(token[len(directLinkTokenPrefix):])
	if err != nil || len(payload) != directLinkTokenSize ||
		subtle.ConstantTimeCompare(payload[:len(directLinkTokenMagic)], directLinkTokenMagic[:]) != 1 ||
		payload[len(directLinkTokenMagic)] != directLinkTokenVersion {
		return nil, false
	}
	return payload, true
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
