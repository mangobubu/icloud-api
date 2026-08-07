package secure

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestCipherRoundTripAndTamper(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	box, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.Encrypt("app-specific-password")
	if err != nil {
		t.Fatal(err)
	}
	if encrypted == "app-specific-password" {
		t.Fatal("密文不应等于明文")
	}
	got, err := box.Decrypt(encrypted)
	if err != nil || got != "app-specific-password" {
		t.Fatalf("解密结果错误: %q, %v", got, err)
	}
	encoded := strings.TrimPrefix(encrypted, "v1.")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 0x01
	tampered := "v1." + base64.RawURLEncoding.EncodeToString(payload)
	if _, err := box.Decrypt(tampered); err == nil {
		t.Fatal("篡改后的密文应解密失败")
	}
}

func TestAppleSessionCipherUsesDedicatedContext(t *testing.T) {
	box, err := NewCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := box.EncryptAppleSession(`{"session_token":"secret"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encrypted, "as1.") {
		t.Fatalf("Apple 会话密文前缀错误: %q", encrypted)
	}
	got, err := box.DecryptAppleSession(encrypted)
	if err != nil || got != `{"session_token":"secret"}` {
		t.Fatalf("Apple 会话解密结果错误: %q, %v", got, err)
	}
	if _, err := box.Decrypt(encrypted); err == nil {
		t.Fatal("Apple 会话密文不应作为 IMAP 凭据解密")
	}
	imapCiphertext, err := box.Encrypt("app-specific-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.DecryptAppleSession(imapCiphertext); err == nil {
		t.Fatal("IMAP 凭据密文不应作为 Apple 会话解密")
	}
}

func TestAPIKeyUsesHighEntropyAndStableHash(t *testing.T) {
	raw, hash, prefix, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 40 || prefix != raw[:12] {
		t.Fatalf("API Key 格式异常: %q", raw)
	}
	if !HashEqual(hash, HashToken(raw)) {
		t.Fatal("API Key 哈希不一致")
	}
}

func TestDirectLinkTokenIsDeterministicVersionedAndBoundToCurrentAPIKeyHash(t *testing.T) {
	box, err := NewCipher(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	apiKeyHash := HashToken("icm_current-key")
	token, err := box.DirectLinkToken(42, apiKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := box.DirectLinkToken(42, apiKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	if token != repeated {
		t.Fatalf("direct-link token is not deterministic: %q != %q", token, repeated)
	}
	if len(token) != 47 || !strings.HasPrefix(token, "icm_") {
		t.Fatalf("direct-link token format = %q, want 47-character icm_ token", token)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(token, "icm_"))
	if err != nil || len(payload) != 32 {
		t.Fatalf("decode direct-link token: len=%d err=%v", len(payload), err)
	}
	if aliasID, ok := DirectLinkTokenAliasID(token); !ok || aliasID != 42 {
		t.Fatalf("parsed direct-link alias = %d, %v; want 42, true", aliasID, ok)
	}
	if !box.VerifyDirectLinkToken(token, 42, apiKeyHash) {
		t.Fatal("direct-link token did not verify against its alias and API key hash")
	}

	rotatedHash := HashToken("icm_rotated-key")
	if box.VerifyDirectLinkToken(token, 42, rotatedHash) {
		t.Fatal("direct-link token remained valid after API key hash rotation")
	}
	if box.VerifyDirectLinkToken(token, 43, apiKeyHash) {
		t.Fatal("direct-link token verified for another alias")
	}
	otherBox, err := NewCipher(bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if otherBox.VerifyDirectLinkToken(token, 42, apiKeyHash) {
		t.Fatal("direct-link token verified with another master key")
	}
	otherAliasToken, err := box.DirectLinkToken(43, apiKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	if otherAliasToken == token {
		t.Fatal("different aliases received the same direct-link token")
	}
}

func TestDirectLinkTokenRejectsMalformedAndTamperedValues(t *testing.T) {
	box, err := NewCipher(bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	apiKeyHash := HashToken("icm_current-key")
	if _, err := box.DirectLinkToken(0, apiKeyHash); err == nil {
		t.Fatal("zero alias ID was accepted")
	}
	if _, err := box.DirectLinkToken(1, []byte("short")); err == nil {
		t.Fatal("short API key hash was accepted")
	}

	token, err := box.DirectLinkToken(7, apiKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "icm_"))
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 0x01
	tamperedMAC := "icm_" + base64.RawURLEncoding.EncodeToString(payload)
	if aliasID, ok := DirectLinkTokenAliasID(tamperedMAC); !ok || aliasID != 7 {
		t.Fatalf("tampered MAC should retain a parseable envelope: id=%d ok=%v", aliasID, ok)
	}
	if box.VerifyDirectLinkToken(tamperedMAC, 7, apiKeyHash) {
		t.Fatal("tampered direct-link MAC verified")
	}

	payload[3]++
	unknownVersion := "icm_" + base64.RawURLEncoding.EncodeToString(payload)
	if _, ok := DirectLinkTokenAliasID(unknownVersion); ok {
		t.Fatal("unknown direct-link token version was accepted")
	}
	if _, ok := DirectLinkTokenAliasID("icm_not-a-token"); ok {
		t.Fatal("malformed direct-link token was accepted")
	}
}
