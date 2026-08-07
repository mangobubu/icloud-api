package apple

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"
)

func TestAppleSRPFixedVector(t *testing.T) {
	// This vector was generated independently with Node's crypto SHA-256 and
	// PBKDF2 plus a small BigInt RFC 5054 implementation, following
	// pyicloud/pysrp's NoUserNameInX formulas. M is sent as Apple's m1 field;
	// H_AMK is sent as Apple's m2 field.
	clientSecret := make([]byte, 32)
	serverSecretBytes := make([]byte, 32)
	for index := range clientSecret {
		clientSecret[index] = byte(index + 1)
		serverSecretBytes[index] = byte(index + 33)
	}
	client, err := newSRPClient(bytes.NewReader(clientSecret))
	if err != nil {
		t.Fatal(err)
	}
	username := []byte("test@example.com")
	salt, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	derived, err := deriveApplePassword("correct horse battery staple", salt, 1000, "s2k_fo")
	if err != nil {
		t.Fatal(err)
	}
	x := srpX(salt, derived)
	verifier := new(big.Int).Exp(appleSRPG, x, appleSRPN)
	serverSecret := new(big.Int).SetBytes(serverSecretBytes)
	serverPublic := new(big.Int).Add(
		new(big.Int).Mul(srpMultiplier(), verifier),
		new(big.Int).Exp(appleSRPG, serverSecret, appleSRPN),
	)
	serverPublic.Mod(serverPublic, appleSRPN)
	if err := client.processChallenge(username, derived, salt, padSRP(serverPublic)); err != nil {
		t.Fatal(err)
	}

	wantDerived := "9601fa505b07f7552bbfa237464cb59ed582258b5672fbc1eb57bd36f284bd55"
	wantA := "630acdff5d334462d92a29e0b7fa6e20020f3333292f6d3a640f1c7a76ad9d317531c57979952e5736c88db118d060dc0539a812b9b0af3b4002380a9f28ae4a7c45a896542de05fbcf76a4e7e0739b9a55d5d6c7aba4f1e1b58729a79bc084d5ff513eaec33ce978f5bad87e579b5a95fc773198e22697b2eadab9eb94f84cdcf1fe94ff09f88d4ca46e968bba443ff71167571f19feb052869bd28d7dabf963b7fe399a1f70e7e08d00e1a3778ed1dddc3325dd09e05d31e774d1fd295c4abfbc613446232004d67cb03d6a034d2ce6ca0a544a0ff5b434b4b4267fa6c6d72acbbda2efc1ef1d1fe36d35382b089abe556862aec35b29d3d0cdf359a9cfed3"
	wantB := "0c5f6ebc72f986145214a2a678a351fb39177e2a49f09d0deff1b70db87e9c6d193566d0cded170511aba1d5dd7b37609c33a7388291b1186a324638729cb6059d9ca14396dda0a2954baf428b44cf65377493ef12935ca050cb09733c6e5f8626646a7021206199c2b95a7bd0c26124ee4707b800149ae899f2305c3ed0c064383042796cd8daa7bc90d07c55679b3b21d10d80d9545b756a4e08c0674d75782dde6809ac54649c859dd5206578c19fffb518a32c61100c68f4d8e16441e5d681da1244066767e35d565d134921f0773fe816a04bf81fcaa994238acc5bedad3db777124220704035f6ecb4aec10ae77408ce048ccad7428e845f3de786522f"
	wantM1 := "e41aa90f640a369544a1bfcea2c47aa3834cf0fd4b6086f28409a76d5dd24eb7"
	wantM2 := "33a593c2c11554f2fd7d43165a6b77a0dd7795c4f722b8110b1933f0b0e8c50f"
	gotDerived := hex.EncodeToString(derived)
	gotA := hex.EncodeToString(client.publicKey())
	gotB := hex.EncodeToString(padSRP(serverPublic))
	gotM1 := hex.EncodeToString(client.m1)
	gotM2 := hex.EncodeToString(client.m2)
	if gotDerived != wantDerived || gotA != wantA || gotB != wantB || gotM1 != wantM1 || gotM2 != wantM2 {
		t.Fatalf("fixed vector mismatch\nderived=%s\nA=%s\nB=%s\nM=%s\nH_AMK=%s", gotDerived, gotA, gotB, gotM1, gotM2)
	}
}

func TestAppleSRPRejectsInvalidInputs(t *testing.T) {
	if _, err := newSRPClient(bytes.NewReader(make([]byte, 32))); err == nil {
		t.Fatal("all-zero client secret was accepted")
	}
	if _, err := deriveApplePassword("password", []byte("salt"), 0, "s2k"); err == nil {
		t.Fatal("zero PBKDF2 iteration count was accepted")
	}
	if _, err := deriveApplePassword("password", []byte("salt"), 1, "unknown"); err == nil {
		t.Fatal("unknown Apple SRP protocol was accepted")
	}
	client, err := newSRPClient(bytes.NewReader(bytes.Repeat([]byte{1}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.processChallenge([]byte("a@example.com"), make([]byte, 32), []byte("salt"), padSRP(big.NewInt(0))); err == nil {
		t.Fatal("zero server public value was accepted")
	}
	if err := client.processChallenge([]byte("a@example.com"), make([]byte, 32), []byte("salt"), padSRP(appleSRPN)); err == nil {
		t.Fatal("server public value N was accepted")
	}
}
