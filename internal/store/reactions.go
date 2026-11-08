package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/router"
)

// L6: emoji reactions, kept on the message they react to, one per person.
const reactionsSchema = `
CREATE TABLE IF NOT EXISTS reactions (
	chat    TEXT NOT NULL,
	msg_id  TEXT NOT NULL, -- WhatsApp id of the message reacted to
	reactor TEXT NOT NULL, -- person key
	name    TEXT NOT NULL,
	emoji   TEXT NOT NULL,
	ts      INTEGER NOT NULL,
	PRIMARY KEY (chat, msg_id, reactor)
);
`

// migrateMessages adds the WhatsApp message id to messages (L6).
func migrateMessages(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('messages')`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	for _, c := range [][2]string{
		{"wa_id", "TEXT NOT NULL DEFAULT ''"},    // L6
		{"to_bot", "INTEGER NOT NULL DEFAULT 0"}, // K7
		{"to_names", "TEXT NOT NULL DEFAULT ''"}, // K7: names, one per line
	} {
		if !have[c[0]] {
			if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN ` + c[0] + ` ` + c[1]); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS messages_wa_id ON messages(chat, wa_id)`)
	return err
}

// SetReaction records reactor's reaction to the message msgID in chat, or
// removes it when emoji is empty. Reactions to messages no longer in the
// window are ignored; ok reports whether the message was found.
func (s *Store) SetReaction(ctx context.Context, chat, msgID, reactor, name, emoji string, at time.Time) (ok bool, err error) {
	if msgID == "" || reactor == "" {
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE chat = ? AND wa_id = ?`, chat, msgID).Scan(&n); err != nil || n == 0 {
		return false, err
	}
	if strings.TrimSpace(emoji) == "" {
		_, err = s.db.ExecContext(ctx, `DELETE FROM reactions WHERE chat = ? AND msg_id = ? AND reactor = ?`, chat, msgID, reactor)
		return true, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO reactions(chat, msg_id, reactor, name, emoji, ts) VALUES (?,?,?,?,?,?)
		ON CONFLICT(chat, msg_id, reactor) DO UPDATE SET name = excluded.name, emoji = excluded.emoji, ts = excluded.ts`,
		chat, msgID, reactor, name, emoji, at.Unix())
	return true, err
}

// loadReactions fills in the reactions of lines (by their WhatsApp id).
func (s *Store) loadReactions(ctx context.Context, chat string, lines []router.Line) error {
	idx := map[string]int{}
	for i, l := range lines {
		if l.ID != "" {
			idx[l.ID] = i
		}
	}
	if len(idx) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT msg_id, reactor, name, emoji FROM reactions WHERE chat = ? ORDER BY ts`, chat)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r router.Reaction
		if err := rows.Scan(&id, &r.SenderID, &r.Name, &r.Emoji); err != nil {
			return err
		}
		if i, ok := idx[id]; ok {
			lines[i].Reactions = append(lines[i].Reactions, r)
		}
	}
	return rows.Err()
}
