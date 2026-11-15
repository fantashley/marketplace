package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"
)

const maxMailAttempts = 8 // ~2 hours of retries with the backoff below

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// sendMail delivers immediately (used for sign-in links, where someone is
// waiting), but still respects the shared send rate.
func (a *App) sendMail(to, subject, body string) error {
	a.throttle()
	return a.send(to, subject, body)
}

// throttle spaces sends at least MailInterval apart across the whole process,
// keeping under the provider's rate limit (Resend: 10/s per team, shared with
// any other app on the same account).
func (a *App) throttle() {
	a.mailMu.Lock()
	defer a.mailMu.Unlock()
	if wait := time.Until(a.nextMailAt); wait > 0 {
		time.Sleep(wait)
	}
	a.nextMailAt = time.Now().Add(a.cfg.MailInterval)
}

func enqueueMail(db execer, to, subject, body string) error {
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO outbox(to_email, subject, body, next_attempt_at, created_at) VALUES(?,?,?,?,?)`,
		to, subject, body, now, now)
	return err
}

// notify queues an email for the outbox worker.
func (a *App) notify(to, subject, body string) {
	if err := enqueueMail(a.db, to, subject, body); err != nil {
		log.Printf("queue mail to %s: %v", to, err)
		return
	}
	a.kickOutbox()
}

func (a *App) kickOutbox() {
	select {
	case a.outboxKick <- struct{}{}:
	default:
	}
}

func (a *App) outboxLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		a.drainOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.outboxKick:
		}
	}
}

func retryDelay(attempts int) time.Duration {
	d := 30 * time.Second << (attempts - 1)
	return min(d, time.Hour)
}

// drainOutbox sends every due message, one at a time. A failed send stays
// queued with exponential backoff; after maxMailAttempts it is dropped and logged.
func (a *App) drainOutbox(ctx context.Context) {
	for ctx.Err() == nil {
		var id int64
		var to, subject, body string
		var attempts int
		err := a.db.QueryRow(`SELECT id, to_email, subject, body, attempts FROM outbox
			WHERE next_attempt_at<=? ORDER BY next_attempt_at, id LIMIT 1`, time.Now().Unix()).
			Scan(&id, &to, &subject, &body, &attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return
		} else if err != nil {
			log.Printf("outbox: %v", err)
			return
		}
		if err := a.sendMail(to, subject, body); err == nil {
			a.db.Exec(`DELETE FROM outbox WHERE id=?`, id)
			continue
		} else {
			attempts++
			if attempts >= maxMailAttempts {
				log.Printf("outbox: giving up on %q to %s after %d attempts: %v", subject, to, attempts, err)
				a.db.Exec(`DELETE FROM outbox WHERE id=?`, id)
				continue
			}
			next := time.Now().Add(retryDelay(attempts))
			log.Printf("outbox: %q to %s failed (attempt %d, retrying %s): %v", subject, to, attempts, next.Format(time.TimeOnly), err)
			a.db.Exec(`UPDATE outbox SET attempts=?, next_attempt_at=?, last_error=? WHERE id=?`, attempts, next.Unix(), err.Error(), id)
		}
	}
}
