package identity

// PA01 (contracts/merchant-password-auth-v1.md §9): UNIT gate for the password primitives. No database,
// no network except a loopback closed port (never dialled: the limiter refuses first).
//
// Red runs (PROCESS §2.4) are recorded in output/auth-core/: mutate VerifyPassword to bytes.Equal ->
// the source check fails; mutate newCodeFrom to `% 10^6` without rejection -> the biased-tail check fails.

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPasswordPA01Crypto(t *testing.T) {
	t.Run("phc round trip", func(t *testing.T) {
		phc, err := HashPassword("correct horse battery")
		if err != nil || !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
			t.Fatalf("phc = %q %v", phc, err)
		}
		other, _ := HashPassword("correct horse battery")
		if phc == other {
			t.Fatal("two hashes of one password share a salt")
		}
		if ok, err := VerifyPassword(phc, "correct horse battery"); !ok || err != nil {
			t.Fatalf("verify right = %v %v", ok, err)
		}
		if ok, err := VerifyPassword(phc, "correct horse batterY"); ok || err != nil {
			t.Fatalf("verify wrong = %v %v", ok, err)
		}
	})

	t.Run("rejects foreign or weak hashes without deriving", func(t *testing.T) {
		good, _ := HashPassword("x")
		parts := strings.Split(good, "$") // "", argon2id, v=19, params, salt, hash
		salt, key := parts[4], parts[5]
		bad := []string{
			"$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
			"$argon2d$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
			"$2a$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW",
			"$argon2id$v=18$m=19456,t=2,p=1$" + salt + "$" + key,
			"$argon2id$v=19$m=65536,t=2,p=1$" + salt + "$" + key,
			"$argon2id$v=19$m=19456,t=3,p=1$" + salt + "$" + key,
			"$argon2id$v=19$m=19456,t=2,p=4$" + salt + "$" + key,
			"$argon2id$v=19$m=8,t=1,p=1$" + salt + "$" + key,
			"$argon2id$v=19$m=19456,t=2,p=1$" + salt[:len(salt)-2] + "$" + key,
			"$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key[:len(key)-2],
			"$argon2id$v=19$m=19456,t=2,p=1$" + salt + "=$" + key,
			"$argon2id$v=19$m=19456,t=2,p=1$" + salt,
			"$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key + "$extra",
			"argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
			"", "plain text",
		}
		before := ArgonCount()
		for _, phc := range bad {
			if ok, err := VerifyPassword(phc, "x"); ok || err == nil {
				t.Fatalf("accepted %q (ok=%v err=%v)", phc, ok, err)
			}
		}
		if ArgonCount() != before {
			t.Fatal("a rejected hash still ran Argon2")
		}
	})

	t.Run("verify compares in constant time", func(t *testing.T) {
		// A behavioural test cannot tell subtle.ConstantTimeCompare from bytes.Equal, so check the source of
		// VerifyPassword (the PA01 red run mutates it to bytes.Equal and this must fail).
		src, err := os.ReadFile("password.go")
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		start := strings.Index(text, "func VerifyPassword(")
		end := strings.Index(text[start:], "\n}\n")
		if start < 0 || end < 0 {
			t.Fatal("VerifyPassword not found")
		}
		body := text[start : start+end]
		if !strings.Contains(body, "subtle.ConstantTimeCompare(") || strings.Contains(body, "bytes.Equal") || strings.Contains(body, "== want") {
			t.Fatal("VerifyPassword must compare with subtle.ConstantTimeCompare")
		}
	})

	t.Run("dummy path runs argon2", func(t *testing.T) {
		dummy, _ := HashPassword(string(bytes.Repeat([]byte{7}, 32)))
		before := ArgonCount()
		if ok, err := VerifyPassword(dummy, "anything at all"); ok || err != nil {
			t.Fatalf("dummy verify = %v %v", ok, err)
		}
		if ArgonCount()-before != 1 {
			t.Fatalf("dummy verify ran %d derivations, want 1", ArgonCount()-before)
		}
	})

	t.Run("limiter: fifth caller waits then busy, argon and hibp share it", func(t *testing.T) {
		old := hashLimiter
		hashLimiter = newLimiter(4, 120*time.Millisecond)
		defer func() { hashLimiter = old }()
		var releases []func()
		for i := 0; i < 4; i++ {
			r, err := hashLimiter.acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			releases = append(releases, r)
		}
		phc := "$argon2id$v=19$m=19456,t=2,p=1$" + strings.Repeat("A", 22) + "$" + strings.Repeat("A", 43)
		hibp := &Passwords{policy: PasswordPolicy{BreachCheck: "hibp"}, hibpBase: "http://127.0.0.1:1", hibp: newHIBPClient()}
		for name, call := range map[string]func() error{
			"hash":   func() error { _, err := HashPassword("x"); return err },
			"verify": func() error { _, err := VerifyPassword(phc, "x"); return err },
			"hibp":   func() error { _, err := hibp.hibpBreached(t.Context(), "x"); return err },
		} {
			start := time.Now()
			err := call()
			if !errors.Is(err, ErrBusy) || time.Since(start) < 100*time.Millisecond {
				t.Fatalf("%s with 4 held slots: err=%v after %v, want ErrBusy after the wait", name, err, time.Since(start))
			}
		}
		// A slot freed during the wait admits the waiter.
		l := newLimiter(1, time.Second)
		hold, _ := l.acquire(t.Context())
		go func() { time.Sleep(30 * time.Millisecond); hold() }()
		if r, err := l.acquire(t.Context()); err != nil {
			t.Fatalf("waiter not admitted: %v", err)
		} else {
			r()
		}
		for _, r := range releases[1:] {
			r()
		}
	})

	t.Run("code generator: 6 digits, uniform, rejects the biased tail", func(t *testing.T) {
		fast := bufio.NewReaderSize(rand.Reader, 1<<16)
		const n = 1_000_000
		var first, last [10]int
		for i := 0; i < n; i++ {
			c, err := newCodeFrom(fast)
			if err != nil || len(c) != 6 || strings.Trim(c, "0123456789") != "" {
				t.Fatalf("code %q %v", c, err)
			}
			first[c[0]-'0']++
			last[c[5]-'0']++
		}
		for name, counts := range map[string][10]int{"first": first, "last": last} {
			chi := 0.0
			for _, c := range counts {
				d := float64(c) - n/10
				chi += d * d / (n / 10)
			}
			// 9 degrees of freedom; 40 is p < 1e-5, so a fair generator essentially never fails.
			if chi > 40 || math.IsNaN(chi) {
				t.Fatalf("%s digit chi-square %.1f over %v", name, chi, counts)
			}
		}
		// Deterministic: a draw in the biased tail (>= 4_294_000_000) must be discarded, not reduced mod 10^6.
		var buf bytes.Buffer
		for _, v := range []uint32{4_294_000_005, 4_294_967_295, 123456, 999_999} {
			_ = binary.Write(&buf, binary.BigEndian, v)
		}
		if c, err := newCodeFrom(&buf); err != nil || c != "123456" {
			t.Fatalf("biased draws not rejected: %q %v", c, err)
		}
		if c, _ := newCodeFrom(&buf); c != "999999" {
			t.Fatalf("next draw = %q, want 999999", c)
		}
		buf.Reset()
		_ = binary.Write(&buf, binary.BigEndian, uint32(4_293_999_999)) // last accepted value
		if c, _ := newCodeFrom(&buf); c != "999999" {
			t.Fatalf("boundary draw = %q, want 999999", c)
		}
		if _, err := newCodeFrom(bytes.NewReader([]byte{1, 2})); err == nil {
			t.Fatal("short entropy source must fail, not loop")
		}
	})

	t.Run("code hmac input is binding then code", func(t *testing.T) {
		pepper := bytes.Repeat([]byte{1}, 32)
		binding := sha256.Sum256([]byte("binding"))
		got := CodeHMAC(pepper, binding[:], "123456")
		m := hmac.New(sha256.New, pepper)
		m.Write(binding[:])
		m.Write([]byte("123456"))
		if !bytes.Equal(got, m.Sum(nil)) || len(got) != 32 {
			t.Fatal("CodeHMAC is not HMAC-SHA256(pepper, binding||code)")
		}
		other := sha256.Sum256([]byte("other"))
		for name, alt := range map[string][]byte{
			"binding": CodeHMAC(pepper, other[:], "123456"),
			"code":    CodeHMAC(pepper, binding[:], "123457"),
			"pepper":  CodeHMAC(bytes.Repeat([]byte{2}, 32), binding[:], "123456"),
		} {
			if bytes.Equal(alt, got) {
				t.Fatalf("hmac ignores %s", name)
			}
		}
	})

	t.Run("email normalization", func(t *testing.T) {
		long := strings.Repeat("a", 254-len("@b.co")) + "@b.co"
		valid := map[string]string{
			"  User@Example.COM ": "user@example.com", "a@b": "a@b", "x+tag@mail.example.test": "x+tag@mail.example.test",
			"first.last@sub.example.test": "first.last@sub.example.test", long: long,
		}
		for in, want := range valid {
			if got, err := NormalizeEmail(in); err != nil || got != want {
				t.Fatalf("NormalizeEmail(%.30q) = %q, %v; want %q", in, got, err, want)
			}
		}
		invalid := []string{"", "no-at", "@x.com", "u@", "a@@b.com", "a@b@c.com", "u@.com", "u@a..com", "u@-a.com", "u@a-.com",
			".a@x.com", "a.@x.com", "a..b@x.com", "a b@x.com", "a<b@x.com", "a>b@x.com", "a,b@x.com", `a"b@x.com`,
			"a@x.com\r\nRCPT TO:<z@z.z>", "üser@example.com", "user@exämple.com", "é@x.com",
			"user@example.com x", strings.Repeat("a", 255-len("@b.co")) + "@b.co"}
		for _, in := range invalid {
			if got, err := NormalizeEmail(in); !errors.Is(err, ErrInvalidEmail) {
				t.Fatalf("NormalizeEmail(%.40q) = %q, %v; want ErrInvalidEmail", in, got, err)
			}
		}
	})

	t.Run("password policy", func(t *testing.T) {
		reason := func(err error) string {
			var pe PolicyError
			if errors.As(err, &pe) {
				return pe.Reason
			}
			if err != nil {
				return "other:" + err.Error()
			}
			return ""
		}
		cases := []struct {
			name, pw, email, want string
		}{
			{"11 chars", strings.Repeat("a", 11), "u@example.test", "too_short"},
			{"12 chars", strings.Repeat("a", 12), "u@example.test", ""},
			{"128 chars", strings.Repeat("a", 128), "u@example.test", ""},
			{"129 chars", strings.Repeat("a", 129), "u@example.test", "too_long"},
			{"12 emoji count as 12", strings.Repeat("\U0001F600", 12), "u@example.test", ""},
			{"11 emoji", strings.Repeat("\U0001F600", 11), "u@example.test", "too_short"},
			{"129 emoji", strings.Repeat("\U0001F600", 129), "u@example.test", "too_long"},
			{"NFC composes before counting", strings.Repeat("é", 6), "u@example.test", "too_short"},
			{"equals email", "merchant.owner@example.test", "merchant.owner@example.test", "equals_email"},
			{"equals email, other case", "Merchant.Owner@Example.Test", "merchant.owner@example.test", "equals_email"},
			{"reset skips equals_email", "merchant.owner@example.test", "", ""},
			{"no composition rules", "aaaaaaaaaaaa", "u@example.test", ""},
		}
		for _, c := range cases {
			if got := reason(CheckPasswordPolicy(c.pw, c.email)); got != c.want {
				t.Fatalf("%s: reason %q, want %q", c.name, got, c.want)
			}
		}
		if strings.Contains(PolicyError{Reason: "too_short"}.Error(), "aaaa") {
			t.Fatal("PolicyError message must never contain the password")
		}
	})
}
