package apple

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math/big"
)

// Apple uses SRP-6a with the RFC 5054 2048-bit group, SHA-256 and its
// NoUserNameInX password derivation variant.
var (
	appleSRPN = mustBigHex(
		"AC6BDB41324A9A9BF166DE5E1389582FAF72B6651987EE07FC3192943DB56050" +
			"A37329CBB4A099ED8193E0757767A13DD52312AB4B03310DCD7F48A9DA04FD50" +
			"E8083969EDB767B0CF6095179A163AB3661A05FBD5FAAAE82918A9962F0B93B8" +
			"55F97993EC975EEAA80D740ADBF4FF747359D041D5C33EA71D281E446B14773B" +
			"CA97B43A23FB801676BD207A436C6481F1D2B9078717461A5B9D32E688F87748" +
			"544523B524B0D57D5EA77A2775D2ECFA032CFBDBF52FB3786160279004E57AE6" +
			"AF874E7303CE53299CCC041C7BC308D82A5698F3A8D0C38271AE35F8E9DBFBB6" +
			"94B5C803D89F7AE435DE236D525F54759B65E372FCD68EF20FA7111F9E4AFF73",
	)
	appleSRPG    = big.NewInt(2)
	appleSRPSize = 256
)

type srpClient struct {
	secret *big.Int
	public *big.Int
	k      *big.Int
	m1     []byte
	m2     []byte
}

func newSRPClient(random io.Reader) (*srpClient, error) {
	secretBytes := make([]byte, 32)
	if _, err := io.ReadFull(random, secretBytes); err != nil {
		return nil, fmt.Errorf("generate SRP secret: %w", err)
	}
	secret := new(big.Int).SetBytes(secretBytes)
	if secret.Sign() == 0 {
		return nil, fmt.Errorf("generate SRP secret: all-zero random value")
	}
	public := new(big.Int).Exp(appleSRPG, secret, appleSRPN)
	return &srpClient{secret: secret, public: public, k: srpMultiplier()}, nil
}

func (c *srpClient) publicKey() []byte {
	return padSRP(c.public)
}

func (c *srpClient) processChallenge(username, passwordKey, salt, serverPublic []byte) error {
	b := new(big.Int).SetBytes(serverPublic)
	if b.Sign() <= 0 || b.Cmp(appleSRPN) >= 0 {
		return fmt.Errorf("invalid SRP server public value")
	}
	x := srpX(salt, passwordKey)
	u := srpU(c.public, b)
	if u.Sign() == 0 {
		return fmt.Errorf("invalid zero SRP scrambling parameter")
	}

	gx := new(big.Int).Exp(appleSRPG, x, appleSRPN)
	base := new(big.Int).Sub(b, new(big.Int).Mul(c.k, gx))
	base.Mod(base, appleSRPN)
	exponent := new(big.Int).Add(c.secret, new(big.Int).Mul(u, x))
	shared := new(big.Int).Exp(base, exponent, appleSRPN)
	key := srpHash(padSRP(shared))
	aBytes := padSRP(c.public)
	bBytes := padSRP(b)
	c.m1 = srpM1(username, salt, aBytes, bBytes, key)
	c.m2 = srpM2(aBytes, c.m1, key)
	return nil
}

func deriveApplePassword(password string, salt []byte, iterations int, protocol string) ([]byte, error) {
	if iterations < 1 || iterations > 10_000_000 {
		return nil, fmt.Errorf("invalid SRP iteration count")
	}
	digest := sha256.Sum256([]byte(password))
	var input string
	switch protocol {
	case "s2k":
		input = string(digest[:])
	case "s2k_fo":
		input = hex.EncodeToString(digest[:])
	default:
		return nil, fmt.Errorf("unsupported SRP protocol %q", protocol)
	}
	return pbkdf2.Key(sha256.New, input, salt, iterations, 32)
}

func srpMultiplier() *big.Int {
	h := sha256.New()
	_, _ = h.Write(appleSRPN.Bytes())
	_, _ = h.Write(padSRP(appleSRPG))
	return hashInt(h)
}

func srpX(salt, passwordKey []byte) *big.Int {
	inner := sha256.New()
	_, _ = inner.Write([]byte(":"))
	_, _ = inner.Write(passwordKey)
	outer := sha256.New()
	_, _ = outer.Write(salt)
	_, _ = outer.Write(inner.Sum(nil))
	return hashInt(outer)
}

func srpU(a, b *big.Int) *big.Int {
	h := sha256.New()
	_, _ = h.Write(padSRP(a))
	_, _ = h.Write(padSRP(b))
	return hashInt(h)
}

func srpM1(username, salt, a, b, key []byte) []byte {
	hN := srpHash(appleSRPN.Bytes())
	hG := srpHash(padSRP(appleSRPG))
	xor := make([]byte, len(hN))
	for index := range xor {
		xor[index] = hN[index] ^ hG[index]
	}
	h := sha256.New()
	_, _ = h.Write(xor)
	_, _ = h.Write(srpHash(username))
	_, _ = h.Write(salt)
	_, _ = h.Write(a)
	_, _ = h.Write(b)
	_, _ = h.Write(key)
	return h.Sum(nil)
}

func srpM2(a, m1, key []byte) []byte {
	h := sha256.New()
	_, _ = h.Write(a)
	_, _ = h.Write(m1)
	_, _ = h.Write(key)
	return h.Sum(nil)
}

func srpHash(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func padSRP(value *big.Int) []byte {
	bytes := value.Bytes()
	if len(bytes) >= appleSRPSize {
		return bytes
	}
	padded := make([]byte, appleSRPSize)
	copy(padded[appleSRPSize-len(bytes):], bytes)
	return padded
}

func hashInt(h hash.Hash) *big.Int {
	return new(big.Int).SetBytes(h.Sum(nil))
}

func mustBigHex(value string) *big.Int {
	result, ok := new(big.Int).SetString(value, 16)
	if !ok {
		panic("invalid SRP group constant")
	}
	return result
}
