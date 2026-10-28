package identity

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	h := HashPassword("Correct-Horse9")
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("unexpected hash format: %s", h)
	}
	if !VerifyPassword("Correct-Horse9", h) {
		t.Fatal("the right password must verify")
	}
	if VerifyPassword("Correct-Horse8", h) {
		t.Fatal("a wrong password must not verify")
	}
	if HashPassword("Correct-Horse9") == h {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
}

func TestVerifyRefusesGarbageAndOverlongInput(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2id$v=19$m=1,t=1,p=1$x$y", "$bcrypt$a$b$c$d$e"} {
		if VerifyPassword("whatever", bad) {
			t.Errorf("garbage hash %q verified", bad)
		}
	}
	h := HashPassword("Correct-Horse9")
	if VerifyPassword(strings.Repeat("a", MaxPasswordLength+1), h) {
		t.Error("an overlong password must be refused before any hashing")
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		name, password string
		want           int
	}{
		{"fine", "Abcdefghi1", 0},
		{"short", "Abc1", 1},
		{"no digit", "Abcdefghij", 1},
		{"no lower", "ABCDEFGHI1", 1},
		{"no upper", "abcdefghi1", 1},
		{"few distinct", "Aa1Aa1Aa1Aa1Aa1", 1},
		{"everything wrong", "aaaa", 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PasswordPolicy(c.password); len(got) != c.want {
				t.Fatalf("PasswordPolicy(%q) = %v, want %d problems", c.password, got, c.want)
			}
		})
	}
}
