package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const adminEmail = "admin@example.com"

type sentMail struct{ to, subject, body string }

type testEnv struct {
	t    *testing.T
	a    *App
	h    http.Handler
	mu   sync.Mutex
	sent []sentMail
	fail func(to string) error // when set, a non-nil result rejects that send
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	a := newApp(Config{
		BaseURL:       "http://test",
		DataDir:       dir,
		SiteTitle:     "Test Stuff",
		AdminEmails:   map[string]bool{adminEmail: true},
		NewItemsDelay: 10 * time.Minute,
	})
	if err := os.MkdirAll(a.photoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a.db = db
	if err := a.loadTemplates(); err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, a: a, h: a.routes()}
	a.send = func(to, subject, body string) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.fail != nil {
			if err := e.fail(to); err != nil {
				return err
			}
		}
		e.sent = append(e.sent, sentMail{to, subject, body})
		return nil
	}
	return e
}

func (e *testEnv) exec(q string, args ...any) sql.Result {
	e.t.Helper()
	res, err := e.a.db.Exec(q, args...)
	if err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return res
}

// user creates an invited, signed-in user and returns their id and session cookie.
func (e *testEnv) user(email, name string) (int64, *http.Cookie) {
	e.t.Helper()
	res := e.exec(`INSERT INTO users(email, name, unsub_token, created_at) VALUES(?,?,?,?)`, email, name, rand.Text(), time.Now().Unix())
	id, _ := res.LastInsertId()
	e.exec(`INSERT OR IGNORE INTO allowed_emails(email) VALUES(?)`, email)
	sid := rand.Text()
	e.exec(`INSERT INTO sessions(hash, user_id, expires_at) VALUES(?,?,?)`, hashToken(sid), id, time.Now().Add(time.Hour).Unix())
	return id, &http.Cookie{Name: sessionCookie, Value: sid}
}

func (e *testEnv) do(method, path string, c *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) doMultipart(path string, c *http.Cookie, fields map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func flash(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "flash" {
			v, _ := url.QueryUnescape(c.Value)
			return v
		}
	}
	return ""
}

func (e *testEnv) drain() { e.a.drainOutbox(context.Background()) }

func (e *testEnv) mailTo(to string) []sentMail {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []sentMail
	for _, m := range e.sent {
		if m.to == to {
			out = append(out, m)
		}
	}
	return out
}

func (e *testEnv) item(id int64) *Item {
	e.t.Helper()
	it, err := e.a.getItem(0, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

// claimedLottery sets up a lottery that closed with winner as the winner and
// the given other entrants.
func (e *testEnv) claimedLottery(winner int64, others ...int64) int64 {
	past := time.Now().Add(-time.Hour).Unix()
	res := e.exec(`INSERT INTO items(title, mode, lottery_ends_at, lottery_secs, status, winner_id, claimed_at, published_at, notified, created_at)
		VALUES('Couch','lottery',?,3600,'claimed',?,?,?,1,?)`, past, winner, past, past, past)
	id, _ := res.LastInsertId()
	for _, u := range append([]int64{winner}, others...) {
		e.exec(`INSERT INTO requests(item_id, user_id, created_at) VALUES(?,?,?)`, id, u, past)
	}
	return id
}

// Review finding: a delayed give-back from the original winner must not take
// the item away from whoever won the redraw.
func TestGiveBackCannotUnclaimRedrawWinner(t *testing.T) {
	e := newTestEnv(t)
	bob, bobC := e.user("bob@example.com", "Bob")
	cat, _ := e.user("cat@example.com", "Cat")
	id := e.claimedLottery(bob, cat)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.do("POST", fmt.Sprintf("/items/%d/release", id), bobC, nil)
		}()
	}
	wg.Wait()

	it := e.item(id)
	if it.Status != "claimed" || it.WinnerID != cat {
		t.Fatalf("after concurrent give-backs: status=%s winner=%d, want claimed by cat (%d)", it.Status, it.WinnerID, cat)
	}
	if len(it.Requests) != 1 || it.Requests[0].UserID != cat {
		t.Fatalf("cat's entry should remain, got %+v", it.Requests)
	}
	// And the contract directly: a stale unclaim for bob is refused.
	if err := e.a.unclaim(id, bob); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale unclaim: got %v, want sql.ErrNoRows", err)
	}
}

func TestStaleAdminReopenIsRefused(t *testing.T) {
	e := newTestEnv(t)
	_, adminC := e.user(adminEmail, "Admin")
	bob, _ := e.user("bob@example.com", "Bob")
	cat, _ := e.user("cat@example.com", "Cat")
	id := e.claimedLottery(bob, cat)
	path := fmt.Sprintf("/admin/items/%d/reopen", id)
	form := url.Values{"winner_id": {fmt.Sprint(bob)}}

	e.do("POST", path, adminC, form) // reopens from bob; redraw picks cat
	rec := e.do("POST", path, adminC, form)
	if it := e.item(id); it.WinnerID != cat {
		t.Fatalf("double-submitted reopen changed the winner to %d, want cat", it.WinnerID)
	}
	if !strings.Contains(flash(rec), "already changed hands") {
		t.Fatalf("flash = %q", flash(rec))
	}
}

// Review finding: rejected sends (e.g. Resend 429s) are retried rather than lost.
func TestDigestRetriesRejectedSends(t *testing.T) {
	e := newTestEnv(t)
	var emails []string
	for i := range 20 {
		email := fmt.Sprintf("friend%d@example.com", i)
		e.user(email, fmt.Sprint("Friend ", i))
		emails = append(emails, email)
	}
	e.exec(`INSERT INTO items(title, mode, status, published_at, created_at) VALUES('Lamp','fcfs','open',?,?)`,
		time.Now().Add(-11*time.Minute).Unix(), time.Now().Unix())

	accepted := 0
	e.fail = func(string) error { // accept the first 10, rate-limit the rest
		if accepted >= 10 {
			return errors.New("resend: 429 Too Many Requests")
		}
		accepted++
		return nil
	}
	e.a.sendNewItemDigest(time.Now())
	e.drain()
	if len(e.sent) != 10 {
		t.Fatalf("first pass sent %d, want 10", len(e.sent))
	}
	var queued int
	e.a.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE attempts=1`).Scan(&queued)
	if queued != 10 {
		t.Fatalf("%d rejected sends queued for retry, want 10", queued)
	}

	e.fail = nil
	e.exec(`UPDATE outbox SET next_attempt_at=0`) // let the backoff elapse
	e.drain()
	for _, email := range emails {
		if got := e.mailTo(email); len(got) != 1 || !strings.Contains(got[0].body, "Lamp") {
			t.Fatalf("%s got %d digests, want exactly 1", email, len(got))
		}
	}
	// No second digest for the same item.
	e.a.sendNewItemDigest(time.Now().Add(time.Hour))
	e.drain()
	if len(e.sent) != 20 {
		t.Fatalf("sent %d emails in total, want 20", len(e.sent))
	}
}

func TestSendsAreThrottled(t *testing.T) {
	e := newTestEnv(t)
	e.a.cfg.MailInterval = 20 * time.Millisecond
	start := time.Now()
	for range 5 {
		e.a.sendMail("x@example.com", "s", "b")
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("5 sends took %s, want them spaced at least 20ms apart", elapsed)
	}
}

func TestFailedSendsGiveUpEventually(t *testing.T) {
	e := newTestEnv(t)
	e.fail = func(string) error { return errors.New("boom") }
	e.a.notify("x@example.com", "s", "b")
	for range maxMailAttempts {
		e.exec(`UPDATE outbox SET next_attempt_at=0`)
		e.drain()
	}
	var n int
	e.a.db.QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	if n != 0 {
		t.Fatalf("outbox still has %d messages after %d failed attempts", n, maxMailAttempts)
	}
}

// Review finding: publishing a draft lottery after its original deadline
// must open a real lottery, not fall straight back to first come.
func TestPublishingDraftLotteryStartsFreshWindow(t *testing.T) {
	e := newTestEnv(t)
	_, adminC := e.user(adminEmail, "Admin")
	rec := e.doMultipart("/admin/items", adminC, map[string]string{
		"title": "Bike", "mode": "lottery", "lottery_hours": "24", "status": "hidden", "handoff": "pickup",
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d", rec.Code)
	}
	e.exec(`UPDATE items SET lottery_ends_at=? WHERE id=1`, time.Now().Add(-24*time.Hour).Unix()) // two days later…

	e.doMultipart("/admin/items/1", adminC, map[string]string{"title": "Bike", "mode": "lottery", "handoff": "pickup"})
	e.a.settleLotteries()
	it := e.item(1)
	if it.Status != "open" || it.Mode != "lottery" {
		t.Fatalf("published draft: status=%s mode=%s, want an open lottery", it.Status, it.Mode)
	}
	if d := time.Until(it.LotteryEndsAt); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("lottery closes in %s, want a fresh 24h window", d)
	}
}

func TestOpenLotteryWithPastDeadlineIsRejected(t *testing.T) {
	e := newTestEnv(t)
	_, adminC := e.user(adminEmail, "Admin")
	// A lottery that fell back to first come; switching it back needs a new length.
	e.exec(`INSERT INTO items(title, mode, lottery_ends_at, status, published_at, created_at) VALUES('Desk','fcfs',?,'open',?,?)`,
		time.Now().Add(-time.Hour).Unix(), time.Now().Unix(), time.Now().Unix())
	rec := e.doMultipart("/admin/items/1", adminC, map[string]string{"title": "Desk", "mode": "lottery"})
	if !strings.Contains(flash(rec), "deadline has passed") {
		t.Fatalf("flash = %q", flash(rec))
	}
	if it := e.item(1); it.Mode != "fcfs" {
		t.Fatalf("mode changed to %s despite the expired deadline", it.Mode)
	}
}

// Review finding: an item hidden before its digest went out is still
// announced once it's published again.
func TestHiddenItemIsAnnouncedAfterRepublishing(t *testing.T) {
	e := newTestEnv(t)
	_, adminC := e.user(adminEmail, "Admin")
	e.user("bob@example.com", "Bob")
	e.exec(`INSERT INTO items(title, mode, status, published_at, created_at) VALUES('Rug','fcfs','hidden',?,?)`,
		time.Now().Add(-20*time.Minute).Unix(), time.Now().Unix())

	e.a.sendNewItemDigest(time.Now())
	e.drain()
	if it := e.item(1); len(e.sent) != 0 {
		t.Fatalf("hidden item %q was announced", it.Title)
	}

	e.doMultipart("/admin/items/1", adminC, map[string]string{"title": "Rug", "mode": "fcfs"})
	e.a.sendNewItemDigest(time.Now())
	e.drain()
	if len(e.sent) != 0 {
		t.Fatal("digest went out before the quiet period after republishing")
	}
	e.a.sendNewItemDigest(time.Now().Add(11 * time.Minute))
	e.drain()
	if got := e.mailTo("bob@example.com"); len(got) != 1 || !strings.Contains(got[0].body, "Rug") {
		t.Fatalf("bob got %+v, want one digest announcing the rug", got)
	}
}

func TestAnnouncedItemIsNotReannouncedAfterHiding(t *testing.T) {
	e := newTestEnv(t)
	_, adminC := e.user(adminEmail, "Admin")
	e.user("bob@example.com", "Bob")
	e.exec(`INSERT INTO items(title, mode, status, published_at, created_at) VALUES('Rug','fcfs','open',?,?)`,
		time.Now().Add(-20*time.Minute).Unix(), time.Now().Unix())
	e.a.sendNewItemDigest(time.Now())
	e.doMultipart("/admin/items/1", adminC, map[string]string{"title": "Rug", "mode": "fcfs", "status": "hidden"})
	e.doMultipart("/admin/items/1", adminC, map[string]string{"title": "Rug", "mode": "fcfs"})
	e.a.sendNewItemDigest(time.Now().Add(time.Hour))
	e.drain()
	if got := e.mailTo("bob@example.com"); len(got) != 1 {
		t.Fatalf("bob got %d digests, want 1", len(got))
	}
}

func TestComments(t *testing.T) {
	e := newTestEnv(t)
	_, adminC := e.user(adminEmail, "Admin")
	bob, bobC := e.user("bob@example.com", "Bob")
	_, catC := e.user("cat@example.com", "Cat")
	_, danC := e.user("dan@example.com", "Dan")
	e.exec(`UPDATE users SET notify_replies=0 WHERE email='dan@example.com'`)
	e.exec(`INSERT INTO items(title, mode, status, published_at, notified, created_at) VALUES('Couch','fcfs','open',?,1,?)`, time.Now().Unix(), time.Now().Unix())
	post := func(c *http.Cookie, body string) *httptest.ResponseRecorder {
		return e.do("POST", "/items/1/comments", c, url.Values{"body": {body}})
	}

	post(bobC, "Does it fit through a <script>alert(1)</script> door?")
	post(danC, "Following along")
	e.drain()
	if got := e.mailTo(adminEmail); len(got) != 2 {
		t.Fatalf("admin got %d emails, want 2 (one per friend comment)", len(got))
	}
	if got := e.mailTo("bob@example.com"); len(got) != 1 || !strings.Contains(got[0].body, "Following along") {
		t.Fatalf("bob should get dan's comment, got %+v", got)
	}

	post(adminC, "Yes, it's 80cm wide.")
	e.drain()
	if got := e.mailTo("bob@example.com"); len(got) != 2 {
		t.Fatalf("bob got %d emails, want the admin's reply too", len(got))
	}
	if got := e.mailTo("dan@example.com"); len(got) != 0 {
		t.Fatalf("dan opted out of reply emails but got %d", len(got))
	}
	if got := e.mailTo("cat@example.com"); len(got) != 0 {
		t.Fatal("cat never commented and shouldn't be emailed")
	}

	page := e.do("GET", "/items/1", catC, nil).Body.String()
	if !strings.Contains(page, "80cm wide") || !strings.Contains(page, "Giving it away") {
		t.Fatal("item page should show the admin's reply, marked as the poster")
	}
	if strings.Contains(page, "<script>alert") {
		t.Fatal("comment body was not escaped")
	}

	var bobComment int64
	e.a.db.QueryRow(`SELECT id FROM comments WHERE user_id=?`, bob).Scan(&bobComment)
	if rec := e.do("POST", fmt.Sprintf("/comments/%d/delete", bobComment), catC, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cat deleting bob's comment: %d, want 404", rec.Code)
	}
	e.do("POST", fmt.Sprintf("/comments/%d/delete", bobComment), bobC, nil)
	var n int
	e.a.db.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d comments left, want 2 after bob deleted his", n)
	}

	post(catC, strings.Repeat("x", maxCommentLen+1))
	e.a.db.QueryRow(`SELECT COUNT(*) FROM comments`).Scan(&n)
	if n != 2 {
		t.Fatal("an over-long comment was saved")
	}

	e.exec(`INSERT INTO items(title, mode, status, created_at) VALUES('Draft','fcfs','hidden',?)`, time.Now().Unix())
	if rec := e.do("POST", "/items/2/comments", catC, url.Values{"body": {"hi"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("commenting on a hidden item: %d, want 404", rec.Code)
	}
}

func TestMigrateAddsColumnsToOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The users and items tables as the first release created them.
	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL DEFAULT '',
		notify_new INTEGER NOT NULL DEFAULT 1, unsub_token TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL);
		CREATE TABLE items (id INTEGER PRIMARY KEY, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL,
		lottery_ends_at INTEGER, status TEXT NOT NULL DEFAULT 'open', winner_id INTEGER, claimed_at INTEGER,
		picked_up INTEGER NOT NULL DEFAULT 0, handoff TEXT NOT NULL DEFAULT 'flexible', handoff_note TEXT NOT NULL DEFAULT '',
		available_from TEXT NOT NULL DEFAULT '', published_at INTEGER, notified INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = openDB(path)
	if err != nil {
		t.Fatalf("opening an old database: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`SELECT notify_replies FROM users; SELECT lottery_secs FROM items`); err != nil {
		t.Fatalf("migrated columns missing: %v", err)
	}
}
