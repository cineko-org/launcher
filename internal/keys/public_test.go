package keys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestParsePublicKeyPEM(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	contents := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	parsed, err := ParsePublicKeyPEM(contents)
	if err != nil || !parsed.Equal(&key.PublicKey) {
		t.Fatalf("parsed = %v, %v", parsed, err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	for _, invalid := range [][]byte{
		nil,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		append(contents, []byte("trailing")...),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("bad")}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rsaDER}),
	} {
		if _, err := ParsePublicKeyPEM(invalid); err == nil {
			t.Fatalf("invalid public key accepted: %q", invalid)
		}
	}
}
