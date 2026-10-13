package password

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=2,p=2$") || strings.Contains(h, "correct") {
		t.Fatalf("unexpected hash format: %s", h)
	}
	if err := Verify("correct horse battery", h); err != nil {
		t.Fatalf("right password rejected: %v", err)
	}
	if err := Verify("correct horse batterz", h); err != ErrMismatch {
		t.Fatalf("wrong password: want ErrMismatch, got %v", err)
	}
}

func TestSaltsAreUnique(t *testing.T) {
	a, _ := Hash("same password here")
	b, _ := Hash("same password here")
	if a == b {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{"", "plaintext", "$argon2i$v=19$m=1,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5", "$argon2id$v=19$m=0,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5", "$argon2id$v=19$m=65536,t=2,p=2$!!$!!"} {
		if err := Verify("x", bad); err == nil || err == ErrMismatch {
			t.Errorf("malformed hash %q should error (not mismatch): %v", bad, err)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	h, _ := Hash("another long password")
	if NeedsRehash(h) {
		t.Fatal("fresh hash should not need rehash")
	}
	weak := "$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5"
	if !NeedsRehash(weak) {
		t.Fatal("weaker parameters should need rehash")
	}
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		pw, user string
		ok       bool
	}{
		{"short", "bob", false},
		{"a-reasonably-long-pass", "bob", true},
		{"my-bobby-password-123", "bobby", false},
		{"aaaaaaaaaaaaaaaa", "x", false},
		{strings.Repeat("ab", 70), "x", false},
	}
	for _, c := range cases {
		if err := Policy(c.pw, c.user); (err == nil) != c.ok {
			t.Errorf("Policy(%q,%q) ok=%v err=%v", c.pw, c.user, c.ok, err)
		}
	}
	if err := Policy("a-reasonably-long-pass", "bob", "a-reasonably-long-pass"); err == nil {
		t.Error("banned password accepted")
	}
}
