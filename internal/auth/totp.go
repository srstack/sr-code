package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TOTP (RFC 6238) two-factor support: a shared secret enrolled into any
// authenticator app via an otpauth:// QR code; login then requires the
// app's current 6-digit code in addition to the password.
//
// The secret lives in totp.json (0600) beside auth.json. Verification
// accepts the current step plus one step of skew on either side.

const totpFileName = "totp.json"

type totpRecord struct {
	Secret string `json:"secret"` // base32, no padding
}

// TotpEnabled reports whether a TOTP secret is enrolled.
func (s *Store) TotpEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totpSecret != nil
}

// TotpEnroll generates (or returns the existing) secret and the otpauth://
// URI to encode as a QR code. issuer/account label the entry in the app.
func (s *Store) TotpEnroll(issuer, account string) (secretBase32, uri string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.totpSecret == nil {
		raw := make([]byte, 20)
		if _, err := rand.Read(raw); err != nil {
			return "", "", err
		}
		s.totpSecret = raw
		if err := s.writeTotpLocked(); err != nil {
			s.totpSecret = nil
			return "", "", err
		}
	}
	secretBase32 = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(s.totpSecret)
	v := url.Values{}
	v.Set("secret", secretBase32)
	v.Set("issuer", issuer)
	v.Set("digits", "6")
	v.Set("period", "30")
	uri = "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
	return secretBase32, uri, nil
}

// TotpRemove disables TOTP and deletes the secret file.
func (s *Store) TotpRemove() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totpSecret = nil
	if err := os.Remove(filepath.Join(s.dataDir, totpFileName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// VerifyTotp reports whether code matches the enrolled secret within ±1 step.
func (s *Store) VerifyTotp(code string) bool {
	s.mu.RLock()
	secret := s.totpSecret
	s.mu.RUnlock()
	if secret == nil {
		return false
	}
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	n, err := strconv.ParseUint(code, 10, 32)
	if err != nil {
		return false
	}
	step := time.Now().Unix() / 30
	for _, offset := range []int64{0, -1, 1} {
		if totpAt(secret, step+offset) == uint32(n) {
			return true
		}
	}
	return false
}

func totpAt(secret []byte, step int64) uint32 {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	truncated := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	return truncated % 1000000
}

func (s *Store) loadTotp() error {
	raw, err := os.ReadFile(filepath.Join(s.dataDir, totpFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var rec totpRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("read totp.json: %w", err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(rec.Secret))
	if err != nil || len(secret) == 0 {
		return errors.New("totp.json: invalid base32 secret")
	}
	s.totpSecret = secret
	return nil
}

func (s *Store) writeTotpLocked() error {
	rec := totpRecord{
		Secret: base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(s.totpSecret),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dataDir, totpFileName), raw, 0o600)
}
