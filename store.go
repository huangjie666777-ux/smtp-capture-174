package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Message struct {
	ID         string
	ReceivedAt time.Time
	Sender     string
	Recipients []string
	Raw        []byte
}

type MessageSummary struct {
	ID         string   `json:"id"`
	ReceivedAt string   `json:"received_at"`
	Sender     string   `json:"sender"`
	Recipients []string `json:"recipients"`
	Size       int64    `json:"size"`
}

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS messages (
	id TEXT PRIMARY KEY,
	received_at TEXT NOT NULL,
	sender TEXT NOT NULL,
	raw BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS message_recipients (
	message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	recipient TEXT NOT NULL,
	recipient_order INTEGER NOT NULL,
	PRIMARY KEY (message_id, recipient)
);
CREATE INDEX IF NOT EXISTS idx_message_recipients_recipient
	ON message_recipients(recipient, message_id);`)
	if err != nil {
		return err
	}
	var columnCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('message_recipients') WHERE name = 'recipient_order'`).Scan(&columnCount); err != nil {
		return err
	}
	if columnCount == 0 {
		_, err = s.db.Exec("ALTER TABLE message_recipients ADD COLUMN recipient_order INTEGER NOT NULL DEFAULT 0")
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SaveMessage(sender string, recipients []string, raw []byte) (string, error) {
	id, err := newMessageID()
	if err != nil {
		return "", err
	}
	receivedAt := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		"INSERT INTO messages (id, received_at, sender, raw) VALUES (?, ?, ?, ?)",
		id, receivedAt.Format(time.RFC3339Nano), sender, raw,
	)
	if err != nil {
		return "", err
	}
	for i, recipient := range recipients {
		_, err = tx.Exec(
			"INSERT INTO message_recipients (message_id, recipient, recipient_order) VALUES (?, ?, ?)",
			id, recipient, i,
		)
		if err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) ListMessages(recipient string) ([]MessageSummary, error) {
	rows, err := s.db.Query(`
SELECT m.id, m.received_at, m.sender, octet_length(m.raw)
FROM messages m
JOIN message_recipients r ON r.message_id = m.id
WHERE r.recipient = ?
ORDER BY m.received_at DESC, m.id DESC`, recipient)
	if err != nil {
		return nil, err
	}
	var result []MessageSummary
	for rows.Next() {
		var item MessageSummary
		if err := rows.Scan(&item.ID, &item.ReceivedAt, &item.Sender, &item.Size); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		recipients, err := s.recipients(result[i].ID)
		if err != nil {
			return nil, err
		}
		result[i].Recipients = recipients
	}
	return result, nil
}

func (s *Store) GetMessage(id string) (*Message, error) {
	if !validMessageID(id) {
		return nil, sql.ErrNoRows
	}
	message := &Message{ID: id}
	var receivedAt string
	err := s.db.QueryRow(
		"SELECT received_at, sender, raw FROM messages WHERE id = ?", id,
	).Scan(&receivedAt, &message.Sender, &message.Raw)
	if err != nil {
		return nil, err
	}
	message.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
	if err != nil {
		return nil, err
	}
	message.Recipients, err = s.recipients(id)
	if err != nil {
		return nil, err
	}
	return message, nil
}

func (s *Store) recipients(id string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT recipient FROM message_recipients WHERE message_id = ? ORDER BY recipient_order", id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recipients []string
	for rows.Next() {
		var recipient string
		if err := rows.Scan(&recipient); err != nil {
			return nil, err
		}
		recipients = append(recipients, recipient)
	}
	return recipients, rows.Err()
}

func newMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexID := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:]), nil
}

func validMessageID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		case r == '-' && (i == 8 || i == 13 || i == 18 || i == 23):
		default:
			return false
		}
	}
	return true
}
