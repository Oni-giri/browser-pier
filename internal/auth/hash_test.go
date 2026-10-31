package auth

import (
	"strings"
	"testing"
)

func TestHashSecret(t *testing.T) {
	h, err := HashSecret("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(h, "correct horse") {
		t.Fatalf("hash %q", h)
	}
	if !VerifySecret(h, "correct horse") {
		t.Fatal("the right secret doesn't verify")
	}
	if VerifySecret(h, "correct horsE") || VerifySecret(h, "") {
		t.Fatal("a wrong secret verifies")
	}
	h2, _ := HashSecret("correct horse")
	if h2 == h {
		t.Fatal("two hashes of the same secret are equal; the salt isn't random")
	}
}

func TestVerifySecretRejectsMalformedHashes(t *testing.T) {
	good, _ := HashSecret("x")
	parts := strings.Split(good, "$")
	for name, h := range map[string]string{
		"empty":      "",
		"bcrypt":     "$2a$10$abcdefghijklmnopqrstuu",
		"argon2i":    strings.Replace(good, "argon2id", "argon2i", 1),
		"old v":      strings.Replace(good, "v=19", "v=16", 1),
		"huge m":     strings.Replace(good, "m=19456", "m=99999999", 1),
		"zero t":     strings.Replace(good, "t=2", "t=0", 1),
		"short key":  strings.Join(append(parts[:5:5], "AAAA"), "$"),
		"bad base64": strings.Join(append(parts[:4:4], "!!!", parts[5]), "$"),
	} {
		if VerifySecret(h, "x") {
			t.Errorf("%s: verified", name)
		}
	}
}
