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
