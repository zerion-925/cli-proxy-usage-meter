package meter

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrClientNotFound = errors.New("API key usage not found")
var ErrClientName = errors.New("invalid API key name")

// A hint is deliberately incomplete even for short keys. It is display metadata,
// never an authentication value; the HMAC ID remains the actual client identity.
func maskedKeyPrefix(key string) string {
	const mask = "••••"
	runes := []rune(key)
	if len(runes) < 12 {
		return mask
	}
	n := min(8, len(runes)/3)
	for _, ch := range runes[:n] {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("-_.:", ch)) {
			return mask
		}
	}
	return string(runes[:n]) + mask
}

func ValidClientName(client, name string) bool {
	if !validClient(client) || client == "" || client == "unattributed" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 {
		return false
	}
	for _, ch := range name {
		if unicode.IsControl(ch) || unicode.In(ch, unicode.Cf) {
			return false
		}
	}
	return true
}

func (s *Store) SetClientName(ctx context.Context, client, name string) error {
	name = strings.TrimSpace(name)
	if !ValidClientName(client, name) {
		return ErrClientName
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if !s.acquire(ctx) {
		return ErrStorage
	}
	defer s.release()
	if s.closed {
		return ErrStorage
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM client_dims WHERE id=?)", client).Scan(&exists); err != nil {
		return ErrStorage
	}
	if !exists {
		return ErrClientNotFound
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO client_labels(client,name) VALUES(?,?) ON CONFLICT(client) DO UPDATE SET name=excluded.name`, client, name); err != nil {
		return ErrStorage
	}
	return nil
}
