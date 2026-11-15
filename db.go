package main

import (
	"database/sql"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY,
	email TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	notify_new INTEGER NOT NULL DEFAULT 1,
	unsub_token TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS allowed_emails (
	email TEXT PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS login_tokens (
	hash TEXT PRIMARY KEY,
	email TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at INTEGER
);
CREATE INDEX IF NOT EXISTS login_tokens_email ON login_tokens(email, created_at);
CREATE TABLE IF NOT EXISTS sessions (
	hash TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS items (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL CHECK (mode IN ('fcfs','lottery','pick')),
	lottery_ends_at INTEGER,
	status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','claimed','hidden')),
	winner_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
	claimed_at INTEGER,
	picked_up INTEGER NOT NULL DEFAULT 0,
	handoff TEXT NOT NULL DEFAULT 'flexible' CHECK (handoff IN ('flexible','pickup','dropoff')),
	handoff_note TEXT NOT NULL DEFAULT '',
	available_from TEXT NOT NULL DEFAULT '', -- YYYY-MM-DD; claimable now, handed off from this date
	published_at INTEGER,          -- first time it became visible
	notified INTEGER NOT NULL DEFAULT 0, -- included in a new-items email yet?
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS photos (
	id INTEGER PRIMARY KEY,
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	filename TEXT NOT NULL,
	position INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS photos_item ON photos(item_id, position);
CREATE TABLE IF NOT EXISTS requests (
	item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	note TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (item_id, user_id)
);
`

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One connection serializes all writes; plenty for a friends-sized app and
	// makes claim races impossible. Never hold a tx while calling a.db directly.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return db, nil
}

type User struct {
	ID        int64
	Email     string
	Name      string
	NotifyNew bool
}

func displayName(name, email string) string {
	if name != "" {
		return name
	}
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

func (u *User) Display() string { return displayName(u.Name, u.Email) }

type Photo struct {
	ID       int64
	Filename string
}

type Request struct {
	UserID    int64
	Name      string
	Email     string
	Note      string
	CreatedAt time.Time
}

func (r Request) Display() string { return displayName(r.Name, r.Email) }

type Item struct {
	ID            int64
	Title         string
	Description   string
	Mode          string
	Status        string
	LotteryEndsAt time.Time
	WinnerID      int64
	WinnerName    string
	WinnerEmail   string
	PickedUp      bool
	Handoff       string // flexible | pickup | dropoff
	HandoffNote   string
	AvailableFrom string // YYYY-MM-DD or ""
	CreatedAt     time.Time
	Photos        []Photo
	RequestCount  int
	Mine          bool // current user has requested / entered
	MyNote        string
	Requests      []Request // populated on the item page
}

func (i *Item) Cover() string {
	if len(i.Photos) == 0 {
		return ""
	}
	return i.Photos[0].Filename
}

var handoffLabels = map[string]string{
	"flexible": "Pickup or drop-off",
	"pickup":   "Must pick up",
	"dropoff":  "I can drop off",
}

func (i *Item) HandoffLabel() string { return handoffLabels[i.Handoff] }

const dateLayout = "2006-01-02"

// AvailableLater reports whether the item can't be handed off until a future date.
func (i *Item) AvailableLater() bool {
	return i.AvailableFrom != "" && i.AvailableFrom > time.Now().Format(dateLayout)
}

// AvailableShort is e.g. "Dec 20"; AvailableLong is e.g. "Saturday, December 20".
func (i *Item) AvailableShort() string { return fmtDate(i.AvailableFrom, "Jan 2") }
func (i *Item) AvailableLong() string  { return fmtDate(i.AvailableFrom, "Monday, January 2") }

func fmtDate(d, layout string) string {
	t, err := time.Parse(dateLayout, d)
	if err != nil {
		return d
	}
	return t.Format(layout)
}

func (i *Item) WinnerDisplay() string { return displayName(i.WinnerName, i.WinnerEmail) }

// LotteryOpen reports whether a lottery item is still taking entries.
func (i *Item) LotteryOpen() bool {
	return i.Mode == "lottery" && i.Status == "open" && time.Now().Before(i.LotteryEndsAt)
}

func unix(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(t, 0)
}

const itemSelect = `
SELECT i.id, i.title, i.description, i.mode, i.status, COALESCE(i.lottery_ends_at,0),
	COALESCE(i.winner_id,0), COALESCE(u.name,''), COALESCE(u.email,''), i.picked_up, i.handoff, i.handoff_note, i.available_from, i.created_at,
	(SELECT COUNT(*) FROM requests r WHERE r.item_id=i.id),
	COALESCE((SELECT r.note FROM requests r WHERE r.item_id=i.id AND r.user_id=?), char(0))
FROM items i LEFT JOIN users u ON u.id=i.winner_id`

func scanItem(sc interface{ Scan(...any) error }) (*Item, error) {
	var it Item
	var ends, created int64
	if err := sc.Scan(&it.ID, &it.Title, &it.Description, &it.Mode, &it.Status, &ends,
		&it.WinnerID, &it.WinnerName, &it.WinnerEmail, &it.PickedUp, &it.Handoff, &it.HandoffNote, &it.AvailableFrom, &created,
		&it.RequestCount, &it.MyNote); err != nil {
		return nil, err
	}
	it.LotteryEndsAt, it.CreatedAt = unix(ends), unix(created)
	if it.MyNote == "\x00" {
		it.MyNote = ""
	} else {
		it.Mine = true
	}
	return &it, nil
}

// listItems returns items for the given viewer. where is an optional SQL filter.
func (a *App) listItems(viewerID int64, where string, args ...any) ([]*Item, error) {
	q := itemSelect
	if where != "" {
		q += " WHERE " + where
	}
	q += ` ORDER BY CASE i.status WHEN 'open' THEN 0 WHEN 'hidden' THEN 1 ELSE 2 END, i.created_at DESC`
	rows, err := a.db.Query(q, append([]any{viewerID}, args...)...)
	if err != nil {
		return nil, err
	}
	var items []*Item
	byID := map[int64]*Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
		byID[it.ID] = it
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	prow, err := a.db.Query(`SELECT id, item_id, filename FROM photos ORDER BY item_id, position, id`)
	if err != nil {
		return nil, err
	}
	defer prow.Close()
	for prow.Next() {
		var p Photo
		var itemID int64
		if err := prow.Scan(&p.ID, &itemID, &p.Filename); err != nil {
			return nil, err
		}
		if it := byID[itemID]; it != nil {
			it.Photos = append(it.Photos, p)
		}
	}
	return items, prow.Err()
}

func (a *App) getItem(viewerID, id int64) (*Item, error) {
	items, err := a.listItems(viewerID, "i.id=?", id)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, sql.ErrNoRows
	}
	it := items[0]
	rows, err := a.db.Query(`SELECT r.user_id, u.name, u.email, r.note, r.created_at
		FROM requests r JOIN users u ON u.id=r.user_id WHERE r.item_id=? ORDER BY r.created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Request
		var created int64
		if err := rows.Scan(&r.UserID, &r.Name, &r.Email, &r.Note, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = unix(created)
		it.Requests = append(it.Requests, r)
	}
	return it, rows.Err()
}

func (a *App) userByID(id int64) (*User, error) {
	var u User
	err := a.db.QueryRow(`SELECT id, email, name, notify_new FROM users WHERE id=?`, id).Scan(&u.ID, &u.Email, &u.Name, &u.NotifyNew)
	return &u, err
}
