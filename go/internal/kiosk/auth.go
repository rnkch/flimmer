package kiosk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/rnkch/flimmer/internal/store/db"
)

const SessionTTL = 30 * 24 * time.Hour

var (
	ErrNoWebPassword    = errors.New("no web password set")
	ErrPasswordTooShort = errors.New("the password needs at least 8 characters")
)

// SetWebPassword stores a bcrypt hash and logs out every session.
func (s *Service) SetWebPassword(ctx context.Context, pw string) error {
	if len(pw) < 8 {
		return ErrPasswordTooShort
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := s.st.PutSetting(ctx, db.PutSettingParams{Key: SettingWebPasswordHash, Value: string(h)}); err != nil {
		return err
	}
	return s.st.DeleteAllSessions(ctx)
}

// ResetWebPassword sets a new random password, logs out every session and
// returns the password. It avoids look-alike characters (0/o, 1/l/i) and is
// grouped as "abcd-efgh-jkmn" so it can be read out over the phone.
func (s *Service) ResetWebPassword(ctx context.Context) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 0, 14)
	for i := 0; i < 12; i++ {
		if i > 0 && i%4 == 0 {
			b = append(b, '-')
		}
		b = append(b, alphabet[randIndex(len(alphabet))])
	}
	pw := string(b)
	return pw, s.SetWebPassword(ctx, pw)
}

func randIndex(n int) int {
	v, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
	return int(v.Int64())
}

// HasWebPassword reports whether a login is possible at all.
func (s *Service) HasWebPassword(ctx context.Context) bool {
	return s.Setting(ctx, SettingWebPasswordHash) != ""
}

// Login checks the password and returns a new session token.
func (s *Service) Login(ctx context.Context, pw string) (string, error) {
	h := s.Setting(ctx, SettingWebPasswordHash)
	if h == "" {
		return "", ErrNoWebPassword
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	s.st.DeleteExpiredSessions(ctx)
	err := s.st.InsertSession(ctx, db.InsertSessionParams{
		TokenHash: hashToken(token),
		ExpiresAt: time.Now().UTC().Add(SessionTTL).Format("2006-01-02T15:04:05Z"),
	})
	return token, err
}

func (s *Service) SessionValid(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	n, err := s.st.SessionValid(ctx, hashToken(token))
	return err == nil && n > 0
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.st.DeleteSession(ctx, hashToken(token))
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}
