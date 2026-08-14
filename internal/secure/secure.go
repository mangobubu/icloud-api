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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

const (
	credentialAAD             = "icloud-api/imap-credential/v1"
	appleSessionAAD           = "icloud-api/apple-web-session/v1"
	pendingAliasAPIKeyAAD     = "icloud-api/pending-alias-api-key/v1"
	aliasCredentialsAAD       = "icloud-api/alias-credentials/v1/"
	directLinkKeyContext      = "icloud-api/direct-link/key/v1"
	directLinkTokenContext    = "icloud-api/direct-link/token/v1"
	directLinkTokenPrefix     = "icm_"
	directLinkTokenVersion    = byte(1)
	recentMailTokenContext    = "icloud-api/direct-link/token/recent-mail/v2"
	recentMailTokenVersion    = byte(2)
	otpTokenContext           = "icloud-api/direct-link/token/otp/v2"
	otpTokenVersion           = byte(3)
	directLinkTokenHeaderSize = 12
	directLinkTokenSize       = 32
	accessTokenKeyContext     = "icloud-api/imap-access-token/key/v1"
	accessTokenContext        = "icloud-api/imap-access-token/token/v1"
	accessTokenPrefix         = "ica_"
	accessTokenVersion        = byte(1)
	accessTokenHeaderSize     = 25
	accessTokenSize           = accessTokenHeaderSize + sha256.Size
)

var directLinkTokenMagic = [3]byte{'d', 'l', 't'}

type Cipher struct {
	aead           cipher.AEAD
	directLinkKey  [sha256.Size]byte
	accessTokenKey [sha256.Size]byte
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
	accessTokenKeyMAC := hmac.New(sha256.New, key)
	_, _ = accessTokenKeyMAC.Write([]byte(accessTokenKeyContext))
	var accessTokenKey [sha256.Size]byte
	copy(accessTokenKey[:], accessTokenKeyMAC.Sum(nil))
	return &Cipher{aead: aead, directLinkKey: directLinkKey, accessTokenKey: accessTokenKey}, nil
}

func (c *Cipher) Encrypt(plaintext string) (string, error) {
	return c.encrypt("v1", credentialAAD, plaintext)
}

func (c *Cipher) EncryptAppleSession(plaintext string) (string, error) {
	return c.encrypt("as1", appleSessionAAD, plaintext)
}

// EncryptPendingAliasAPIKey preserves the one-time key issued by the legacy
// automatic-alias flow until an administrator acknowledges it.
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

// EncryptAliasCredentials binds one full credential bundle to its alias ID.
// Moving ciphertext between rows therefore fails authentication.
func (c *Cipher) EncryptAliasCredentials(aliasID int64, credentials domain.AliasCredentials) (string, error) {
	if aliasID < 1 {
		return "", errors.New("隐私邮箱 ID 必须是正整数")
	}
	if err := validateAliasCredentials(credentials); err != nil {
		return "", err
	}
	payload, err := json.Marshal(credentials)
	if err != nil {
		return "", fmt.Errorf("编码隐私邮箱凭证: %w", err)
	}
	return c.encrypt("mc1", aliasCredentialsAAD+strconv.FormatInt(aliasID, 10), string(payload))
}

func (c *Cipher) DecryptAliasCredentials(aliasID int64, value string) (domain.AliasCredentials, error) {
	if aliasID < 1 {
		return domain.AliasCredentials{}, errors.New("隐私邮箱 ID 必须是正整数")
	}
	payload, err := c.decrypt("mc1", aliasCredentialsAAD+strconv.FormatInt(aliasID, 10), value)
	if err != nil {
		return domain.AliasCredentials{}, err
	}
	var credentials domain.AliasCredentials
	if err := json.Unmarshal([]byte(payload), &credentials); err != nil {
		return domain.AliasCredentials{}, errors.New("隐私邮箱凭证格式错误")
	}
	if err := validateAliasCredentials(credentials); err != nil {
		return domain.AliasCredentials{}, err
	}
	return credentials, nil
}

func validateAliasCredentials(credentials domain.AliasCredentials) error {
	values := []string{credentials.APIKey, credentials.IMAPPassword, credentials.ClientID, credentials.RefreshToken}
	for _, value := range values {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
			return errors.New("隐私邮箱凭证不完整或格式错误")
		}
	}
	if !validGeneratedToken(credentials.APIKey, "icm_", 32) ||
		!validGeneratedToken(credentials.IMAPPassword, "imp_", 32) ||
		!validGeneratedToken(credentials.ClientID, "icl_", 18) ||
		!validGeneratedToken(credentials.RefreshToken, "icr_", 32) {
		return errors.New("隐私邮箱凭证格式错误")
	}
	return nil
}

func validGeneratedToken(value, prefix string, byteLength int) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value[len(prefix):])
	return err == nil && len(decoded) == byteLength
}

func NewAliasCredentials() (domain.AliasCredentials, error) {
	apiKey, err := RandomToken(32)
	if err != nil {
		return domain.AliasCredentials{}, fmt.Errorf("生成 API Key: %w", err)
	}
	password, err := RandomToken(32)
	if err != nil {
		return domain.AliasCredentials{}, fmt.Errorf("生成 IMAP 密码: %w", err)
	}
	clientID, err := RandomToken(18)
	if err != nil {
		return domain.AliasCredentials{}, fmt.Errorf("生成 client ID: %w", err)
	}
	refreshToken, err := RandomToken(32)
	if err != nil {
		return domain.AliasCredentials{}, fmt.Errorf("生成刷新令牌: %w", err)
	}
	return domain.AliasCredentials{
		APIKey:       "icm_" + apiKey,
		IMAPPassword: "imp_" + password,
		ClientID:     "icl_" + clientID,
		RefreshToken: "icr_" + refreshToken,
	}, nil
}

func NewAliasCredentialMaterial(c *Cipher, aliasID, version int64) (domain.AliasCredentials, domain.AliasCredentialMaterial, error) {
	if c == nil {
		return domain.AliasCredentials{}, domain.AliasCredentialMaterial{}, errors.New("凭据加密器不可用")
	}
	if version < 1 {
		version = 1
	}
	credentials, err := NewAliasCredentials()
	if err != nil {
		return domain.AliasCredentials{}, domain.AliasCredentialMaterial{}, err
	}
	ciphertext, err := c.EncryptAliasCredentials(aliasID, credentials)
	if err != nil {
		return domain.AliasCredentials{}, domain.AliasCredentialMaterial{}, err
	}
	material := domain.AliasCredentialMaterial{
		Ciphertext:       ciphertext,
		APIKeyHash:       HashToken(credentials.APIKey),
		APIKeyPrefix:     credentials.APIKey[:12],
		IMAPPasswordHash: HashToken(credentials.IMAPPassword),
		OAuthClientID:    credentials.ClientID,
		RefreshTokenHash: HashToken(credentials.RefreshToken),
		Version:          version,
	}
	return credentials, material, nil
}

// NewAliasCredentialMaterialWithAPIKey issues the non-legacy portion of a
// credential bundle while retaining an already published API Key. This is
// used only when a pending legacy key can still be decrypted during upgrade.
func NewAliasCredentialMaterialWithAPIKey(c *Cipher, aliasID, version int64, apiKey string) (domain.AliasCredentials, domain.AliasCredentialMaterial, error) {
	if c == nil {
		return domain.AliasCredentials{}, domain.AliasCredentialMaterial{}, errors.New("凭据加密器不可用")
	}
	if !validGeneratedToken(apiKey, "icm_", 32) {
		return domain.AliasCredentials{}, domain.AliasCredentialMaterial{}, errors.New("API Key 格式错误")
	}
	if version < 1 {
		version = 1
	}
	credentials, err := NewAliasCredentials()
	if err != nil {
		return domain.AliasCredentials{}, domain.AliasCredentialMaterial{}, err
	}
	credentials.APIKey = apiKey
	ciphertext, err := c.EncryptAliasCredentials(aliasID, credentials)
	if err != nil {
		return domain.AliasCredentials{}, domain.AliasCredentialMaterial{}, err
	}
	return credentials, domain.AliasCredentialMaterial{
		Ciphertext:       ciphertext,
		APIKeyHash:       HashToken(apiKey),
		APIKeyPrefix:     apiKey[:12],
		IMAPPasswordHash: HashToken(credentials.IMAPPassword),
		OAuthClientID:    credentials.ClientID,
		RefreshTokenHash: HashToken(credentials.RefreshToken),
		Version:          version,
	}, nil
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

// NewAPIKey creates the legacy administrator-visible API key material. The
// raw value is returned exactly once; persistence stores only its hash and an
// encrypted copy for the pending auto-creation delivery queue.
func NewAPIKey() (raw string, hash []byte, prefix string, err error) {
	secret, err := RandomToken(32)
	if err != nil {
		return "", nil, "", err
	}
	raw = "icm_" + secret
	return raw, HashToken(raw), raw[:12], nil
}

// DirectLinkToken preserves the original v1 URL credential for legacy
// aliases. New v2 callers must use RecentMailToken or OTPToken so a token
// disclosed for one route cannot authorize the other route.
func (c *Cipher) DirectLinkToken(aliasID int64, apiKeyHash []byte) (string, error) {
	return c.directLinkToken(aliasID, apiKeyHash, directLinkTokenVersion, directLinkTokenContext)
}

// RecentMailToken derives a v2 URL credential that can authorize only the
// consuming recent-mail route.
func (c *Cipher) RecentMailToken(aliasID int64, apiKeyHash []byte) (string, error) {
	return c.directLinkToken(aliasID, apiKeyHash, recentMailTokenVersion, recentMailTokenContext)
}

// OTPToken derives a v2 URL credential that can authorize only the repeatable
// OTP-history route.
func (c *Cipher) OTPToken(aliasID int64, apiKeyHash []byte) (string, error) {
	return c.directLinkToken(aliasID, apiKeyHash, otpTokenVersion, otpTokenContext)
}

func (c *Cipher) directLinkToken(aliasID int64, apiKeyHash []byte, version byte, context string) (string, error) {
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
	payload[len(directLinkTokenMagic)] = version
	binary.BigEndian.PutUint64(payload[4:directLinkTokenHeaderSize], uint64(aliasID))

	mac := hmac.New(sha256.New, c.directLinkKey[:])
	_, _ = mac.Write([]byte(context))
	_, _ = mac.Write(payload[:directLinkTokenHeaderSize])
	_, _ = mac.Write(apiKeyHash)
	copy(payload[directLinkTokenHeaderSize:], mac.Sum(nil))

	return directLinkTokenPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

// DirectLinkTokenAliasID recognizes only the legacy v1 direct-link envelope
// and returns its untrusted alias ID. Call VerifyDirectLinkToken with the
// current stored API key hash before accepting the credential.
func DirectLinkTokenAliasID(token string) (int64, bool) {
	return directLinkTokenAliasID(token, directLinkTokenVersion)
}

// RecentMailTokenAliasID recognizes only a recent-mail-purpose token and
// returns its untrusted alias ID.
func RecentMailTokenAliasID(token string) (int64, bool) {
	return directLinkTokenAliasID(token, recentMailTokenVersion)
}

// OTPTokenAliasID recognizes only an OTP-purpose token and returns its
// untrusted alias ID.
func OTPTokenAliasID(token string) (int64, bool) {
	return directLinkTokenAliasID(token, otpTokenVersion)
}

func directLinkTokenAliasID(token string, version byte) (int64, bool) {
	payload, ok := directLinkTokenPayload(token, version)
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
	return c.verifyDirectLinkToken(token, aliasID, apiKeyHash, directLinkTokenVersion, directLinkTokenContext)
}

// VerifyRecentMailToken authenticates a recent-mail-purpose token against the
// alias's current API key hash.
func (c *Cipher) VerifyRecentMailToken(token string, aliasID int64, apiKeyHash []byte) bool {
	return c.verifyDirectLinkToken(token, aliasID, apiKeyHash, recentMailTokenVersion, recentMailTokenContext)
}

// VerifyOTPToken authenticates an OTP-purpose token against the alias's
// current API key hash.
func (c *Cipher) VerifyOTPToken(token string, aliasID int64, apiKeyHash []byte) bool {
	return c.verifyDirectLinkToken(token, aliasID, apiKeyHash, otpTokenVersion, otpTokenContext)
}

func (c *Cipher) verifyDirectLinkToken(
	token string,
	aliasID int64,
	apiKeyHash []byte,
	version byte,
	context string,
) bool {
	payload, ok := directLinkTokenPayload(token, version)
	if !ok || c == nil || len(apiKeyHash) != sha256.Size {
		return false
	}
	parsedID, ok := directLinkTokenAliasID(token, version)
	if !ok || parsedID != aliasID {
		return false
	}
	expected, err := c.directLinkToken(aliasID, apiKeyHash, version, context)
	if err != nil {
		return false
	}
	expectedPayload, ok := directLinkTokenPayload(expected, version)
	return ok && hmac.Equal(payload[directLinkTokenHeaderSize:], expectedPayload[directLinkTokenHeaderSize:])
}

// IssueAliasAccessToken creates a short-lived opaque bearer token for IMAP
// XOAUTH2. The MAC is bound to the current refresh-token hash and credential
// version, so a bundle rotation invalidates every outstanding access token.
func (c *Cipher) IssueAliasAccessToken(
	aliasID, credentialVersion int64,
	refreshTokenHash []byte,
	expiresAt time.Time,
) (string, error) {
	if c == nil || aliasID < 1 || credentialVersion < 1 || len(refreshTokenHash) != sha256.Size {
		return "", errors.New("访问令牌凭据无效")
	}
	if expiresAt.IsZero() || expiresAt.Unix() <= 0 {
		return "", errors.New("访问令牌过期时间无效")
	}
	payload := make([]byte, accessTokenSize)
	payload[0] = accessTokenVersion
	binary.BigEndian.PutUint64(payload[1:9], uint64(aliasID))
	binary.BigEndian.PutUint64(payload[9:17], uint64(credentialVersion))
	binary.BigEndian.PutUint64(payload[17:25], uint64(expiresAt.Unix()))
	mac := hmac.New(sha256.New, c.accessTokenKey[:])
	_, _ = mac.Write([]byte(accessTokenContext))
	_, _ = mac.Write(payload[:accessTokenHeaderSize])
	_, _ = mac.Write(refreshTokenHash)
	copy(payload[accessTokenHeaderSize:], mac.Sum(nil))
	return accessTokenPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func AliasAccessTokenIdentity(token string) (aliasID, credentialVersion int64, expiresAt time.Time, ok bool) {
	payload, ok := aliasAccessTokenPayload(token)
	if !ok {
		return 0, 0, time.Time{}, false
	}
	rawAliasID := binary.BigEndian.Uint64(payload[1:9])
	rawVersion := binary.BigEndian.Uint64(payload[9:17])
	rawExpiry := binary.BigEndian.Uint64(payload[17:25])
	if rawAliasID == 0 || rawAliasID > uint64(^uint64(0)>>1) ||
		rawVersion == 0 || rawVersion > uint64(^uint64(0)>>1) ||
		rawExpiry == 0 || rawExpiry > uint64(^uint64(0)>>1) {
		return 0, 0, time.Time{}, false
	}
	return int64(rawAliasID), int64(rawVersion), time.Unix(int64(rawExpiry), 0).UTC(), true
}

func (c *Cipher) VerifyAliasAccessToken(
	token string,
	aliasID, credentialVersion int64,
	refreshTokenHash []byte,
	now time.Time,
) bool {
	parsedAliasID, parsedVersion, expiresAt, ok := AliasAccessTokenIdentity(token)
	if !ok || c == nil || parsedAliasID != aliasID || parsedVersion != credentialVersion ||
		len(refreshTokenHash) != sha256.Size || !now.UTC().Before(expiresAt) {
		return false
	}
	expected, err := c.IssueAliasAccessToken(aliasID, credentialVersion, refreshTokenHash, expiresAt)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func aliasAccessTokenPayload(token string) ([]byte, bool) {
	if !strings.HasPrefix(token, accessTokenPrefix) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(token[len(accessTokenPrefix):])
	if err != nil || len(payload) != accessTokenSize || payload[0] != accessTokenVersion {
		return nil, false
	}
	return payload, true
}

func directLinkTokenPayload(token string, version byte) ([]byte, bool) {
	if len(token) != len(directLinkTokenPrefix)+43 || !strings.HasPrefix(token, directLinkTokenPrefix) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(token[len(directLinkTokenPrefix):])
	if err != nil || len(payload) != directLinkTokenSize ||
		subtle.ConstantTimeCompare(payload[:len(directLinkTokenMagic)], directLinkTokenMagic[:]) != 1 ||
		payload[len(directLinkTokenMagic)] != version {
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
