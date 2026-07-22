package auth

import "testing"

// RFC 6238 Appendix B test vectors (SHA1, 8-digit codes).
func TestTotpAtRFC6238(t *testing.T) {
	secret := []byte("12345678901234567890")
	cases := []struct {
		step int64
		want uint32
	}{
		{59 / 30, 94287082 % 1000000},
		{1111111109 / 30, 7081804 % 1000000},
		{1111111111 / 30, 14050471 % 1000000},
		{1234567890 / 30, 89005924 % 1000000},
	}
	for _, c := range cases {
		if got := totpAt(secret, c.step); got != c.want {
			t.Errorf("totpAt(step=%d) = %d, want %d", c.step, got, c.want)
		}
	}
}
