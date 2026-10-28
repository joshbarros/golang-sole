package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
)

// MaxPasswordLength: hashing cost grows with the input, so an absurdly long password is refused before anything is hashed.
const MaxPasswordLength = 128

const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB (the OWASP minimum for argon2id)
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword returns a self-describing argon2id hash.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// VerifyPassword compares a password with a stored hash in constant time.
func VerifyPassword(password, encoded string) bool {
	if len(password) > MaxPasswordLength {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version, memory, time, threads int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, uint32(time), uint32(memory), uint8(threads), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// PasswordPolicy returns what is wrong with a password, in words a person can act on (empty means it is acceptable):
// 10 or more characters, an upper case letter, a lower case letter, a digit, and 4 different characters.
func PasswordPolicy(password string) []string {
	var problems []string
	if len([]rune(password)) < 10 {
		problems = append(problems, "Passwords must be at least 10 characters.")
	}
	var digit, lower, upper bool
	distinct := map[rune]struct{}{}
	for _, c := range password {
		distinct[c] = struct{}{}
		switch {
		case unicode.IsDigit(c):
			digit = true
		case unicode.IsLower(c):
			lower = true
		case unicode.IsUpper(c):
			upper = true
		}
	}
	if !digit {
		problems = append(problems, "Passwords must have at least one digit ('0'-'9').")
	}
	if !lower {
		problems = append(problems, "Passwords must have at least one lowercase ('a'-'z').")
	}
	if !upper {
		problems = append(problems, "Passwords must have at least one uppercase ('A'-'Z').")
	}
	if len(distinct) < 4 {
		problems = append(problems, "Passwords must use at least 4 different characters.")
	}
	return problems
}
