package defaults

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/notify"
	"github.com/w3ft-net/weft/shape"
)

// NotifyBackend is the notify.yaml document and the secrets over one SQLite database.
type NotifyBackend struct {
	db  *sql.DB
	gcm cipher.AEAD // nil: secrets are stored as they are
}

var (
	_ notify.Store   = (*NotifyBackend)(nil)
	_ notify.Secrets = (*NotifyBackend)(nil)
)

const notifySchema = `
CREATE TABLE IF NOT EXISTS notify_aliases (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	text TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	authored_by TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notify_secrets (
	name TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Times are stored as fixed-width UTC text.
const notifyTimeFmt = "2006-01-02T15:04:05.000000000Z"

// NewNotifyBackend is a store over db, creating its tables if need be. db comes from
// sqlitedb.Open and is the caller's to close.
//
// Secret values are encrypted at rest with AES-256-GCM when WEFT_SECRETS_KEY is
// set (any string; its SHA-256 is the key). Without it they are stored in clear
// and the backend warns once at startup. A value written before the key was set
// still reads; a stored ciphertext does not read without the key it was written
// with.
func NewNotifyBackend(db *sql.DB) (*NotifyBackend, error) {
	if _, err := db.Exec(notifySchema); err != nil {
		return nil, fmt.Errorf("create notify schema: %w", err)
	}
	s := &NotifyBackend{db: db}
	if k := os.Getenv("WEFT_SECRETS_KEY"); k != "" {
		g, err := newSecretsGCM(k)
		if err != nil {
			return nil, err
		}
		s.gcm = g
	} else {
		weftsdk.Warnf(context.Background(), "notify secrets are stored unencrypted: set WEFT_SECRETS_KEY to encrypt them at rest")
	}
	return s, nil
}

func newSecretsGCM(key string) (cipher.AEAD, error) {
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

const secretPrefix = "enc:v1:"

// seal is the stored form of a secret's value. The name is authenticated with
// it, so a ciphertext copied under another name does not open.
func (s *NotifyBackend) seal(name, value string) (string, error) {
	if s.gcm == nil {
		return value, nil
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := s.gcm.Seal(nonce, nonce, []byte(value), []byte(name))
	return secretPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

func (s *NotifyBackend) open(name, stored string) (string, error) {
	enc, ok := strings.CutPrefix(stored, secretPrefix)
	if !ok {
		return stored, nil
	}
	if s.gcm == nil {
		return "", fmt.Errorf("secret %q is encrypted: set WEFT_SECRETS_KEY to the key it was written with", name)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < s.gcm.NonceSize() {
		return "", fmt.Errorf("secret %q is corrupt", name)
	}
	n := s.gcm.NonceSize()
	pt, err := s.gcm.Open(nil, raw[:n], raw[n:], []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %q does not decrypt with WEFT_SECRETS_KEY", name)
	}
	return string(pt), nil
}

func (s *NotifyBackend) Aliases(shape.Added) (notify.Document, error) {
	var text, updated string
	err := s.db.QueryRow(`SELECT text, updated_at FROM notify_aliases WHERE id = 1`).Scan(&text, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return notify.Document{}, nil
	}
	if err != nil {
		return notify.Document{}, err
	}
	at, err := time.Parse(notifyTimeFmt, updated)
	return notify.Document{Text: text, UpdatedAt: at}, err
}

func (s *NotifyBackend) SaveAliases(_ shape.Added, user, text string) error {
	now := time.Now().UTC().Format(notifyTimeFmt)
	var cur string
	err := s.db.QueryRow(`SELECT text FROM notify_aliases WHERE id = 1`).Scan(&cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(`INSERT INTO notify_aliases (id, text, authored_by, updated_at) VALUES (1, ?, ?, ?)`, text, user, now)
		return err
	case err != nil:
		return err
	case cur == text:
		return nil
	}
	// An edit sets who wrote the version and clears its message.
	_, err = s.db.Exec(`UPDATE notify_aliases SET text = ?, message = '', authored_by = ?, updated_at = ? WHERE id = 1`, text, user, now)
	return err
}

func (s *NotifyBackend) AmendMessage(_ shape.Added, authorID, message string) error {
	var author string
	err := s.db.QueryRow(`SELECT authored_by FROM notify_aliases WHERE id = 1`).Scan(&author)
	if errors.Is(err, sql.ErrNoRows) {
		return notify.ErrNotFound
	}
	if err != nil {
		return err
	}
	if authorID != "" && author != authorID {
		return notify.ErrNotYours
	}
	_, err = s.db.Exec(`UPDATE notify_aliases SET message = ? WHERE id = 1`, message)
	return err
}

func (s *NotifyBackend) Names(shape.Added) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM notify_secrets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, rows.Err()
}

func (s *NotifyBackend) Value(_ shape.Added, name string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM notify_secrets WHERE name = ?`, name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", notify.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return s.open(name, v)
}

func (s *NotifyBackend) Set(_ shape.Added, name, value string) error {
	stored, err := s.seal(name, value)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO notify_secrets (name, value) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value`, name, stored)
	return err
}

func (s *NotifyBackend) Delete(_ shape.Added, name string) error {
	res, err := s.db.Exec(`DELETE FROM notify_secrets WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notify.ErrNotFound
	}
	return nil
}
