package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"strings"
	"time"
)

func (a *App) backgroundLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		a.settleLotteries()
		a.sendNewItemDigest(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// settleLotteries draws every lottery whose window has closed.
func (a *App) settleLotteries() {
	rows, err := a.db.Query(`SELECT id FROM items WHERE status='open' AND mode='lottery' AND lottery_ends_at<=?`, time.Now().Unix())
	if err != nil {
		log.Printf("settle: %v", err)
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := a.draw(id); err != nil {
			log.Printf("draw item %d: %v", id, err)
		}
	}
}

func randIndex(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		panic(err)
	}
	return int(v.Int64())
}

// draw picks a uniformly random entrant for a closed lottery. With no
// entrants the item falls back to first come, first served.
func (a *App) draw(itemID int64) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, mode string
	var ends int64
	if err := tx.QueryRow(`SELECT status, mode, COALESCE(lottery_ends_at,0) FROM items WHERE id=?`, itemID).
		Scan(&status, &mode, &ends); err != nil {
		return err
	}
	if status != "open" || mode != "lottery" || ends > time.Now().Unix() {
		return nil
	}
	rows, err := tx.Query(`SELECT user_id FROM requests WHERE item_id=?`, itemID)
	if err != nil {
		return err
	}
	var entrants []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		entrants = append(entrants, id)
	}
	rows.Close()
	if len(entrants) == 0 {
		if _, err := tx.Exec(`UPDATE items SET mode='fcfs' WHERE id=?`, itemID); err != nil {
			return err
		}
		return tx.Commit()
	}
	winner := entrants[randIndex(len(entrants))]
	if _, err := tx.Exec(`UPDATE items SET status='claimed', winner_id=?, claimed_at=? WHERE id=?`,
		winner, time.Now().Unix(), itemID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("item %d: lottery drawn among %d, winner user %d", itemID, len(entrants), winner)
	a.notifyWinner(itemID, winner, fmt.Sprintf("You won the lottery (%d entered)!", len(entrants)))
	return nil
}

// unclaim puts a claimed item back up. The previous winner's request is
// dropped so they can't win it again. Lotteries redraw immediately among the
// remaining entrants (or fall back to first come, first served).
func (a *App) unclaim(itemID int64) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var mode string
	var winner sql.NullInt64
	if err := tx.QueryRow(`SELECT mode, winner_id FROM items WHERE id=? AND status='claimed'`, itemID).Scan(&mode, &winner); err != nil {
		return err
	}
	if winner.Valid {
		tx.Exec(`DELETE FROM requests WHERE item_id=? AND user_id=?`, itemID, winner.Int64)
	}
	q := `UPDATE items SET status='open', winner_id=NULL, claimed_at=NULL, picked_up=0 WHERE id=?`
	if mode == "lottery" {
		q = `UPDATE items SET status='open', winner_id=NULL, claimed_at=NULL, picked_up=0, lottery_ends_at=MIN(lottery_ends_at, ?) WHERE id=?`
		_, err = tx.Exec(q, time.Now().Unix(), itemID)
	} else {
		_, err = tx.Exec(q, itemID)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if mode == "lottery" {
		return a.draw(itemID)
	}
	return nil
}

func (a *App) notifyWinner(itemID, userID int64, headline string) {
	u, err := a.userByID(userID)
	if err != nil {
		return
	}
	it, err := a.getItem(0, itemID)
	if err != nil {
		return
	}
	title := it.Title
	body := fmt.Sprintf("%s\n\n%s is yours.\nHandoff: %s", headline, title, it.HandoffLabel())
	if it.HandoffNote != "" {
		body += " — " + it.HandoffNote
	}
	if it.AvailableLater() {
		body += "\nAvailable from: " + it.AvailableLong() + " (I'm still using it until then)"
	}
	body += fmt.Sprintf("\n\n%s/items/%d\n\nReply to this email or message me to sort out the handoff. "+
		"If you don't want it after all, tap \"Give it back\" on the item page so it goes to someone else.\n",
		a.cfg.BaseURL, itemID)
	a.notify(u.Email, "You got: "+title, body)
}

func (a *App) notifyAdmins(subject, body string) {
	for email := range a.cfg.AdminEmails {
		a.notify(email, subject, body)
	}
}

// sendNewItemDigest emails everyone who opted in about items published since
// the last digest, once posting has been quiet for newItemsQuietPeriod.
func (a *App) sendNewItemDigest(now time.Time) {
	var pending int
	var latest int64
	a.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(published_at),0) FROM items WHERE notified=0 AND published_at IS NOT NULL`).
		Scan(&pending, &latest)
	if pending == 0 || now.Sub(unix(latest)) < a.cfg.NewItemsDelay {
		return
	}
	items, err := a.listItems(0, "i.notified=0 AND i.published_at IS NOT NULL AND i.status='open'")
	if err != nil {
		log.Printf("digest: %v", err)
		return
	}
	// Mark first: a crash after this skips one digest rather than repeating it.
	if _, err := a.db.Exec(`UPDATE items SET notified=1 WHERE notified=0 AND published_at IS NOT NULL AND published_at<=?`, latest); err != nil {
		log.Printf("digest: %v", err)
		return
	}
	if len(items) == 0 { // everything got claimed before the email went out
		return
	}
	var list strings.Builder
	for _, it := range items {
		avail := ""
		if it.AvailableLater() {
			avail = " · available from " + it.AvailableShort()
		}
		fmt.Fprintf(&list, "• %s\n  %s · %s%s\n  %s/items/%d\n\n", it.Title, a.modeSummary(it), it.HandoffLabel(), avail, a.cfg.BaseURL, it.ID)
	}
	subject := fmt.Sprintf("New on %s: %s", a.cfg.SiteTitle, items[0].Title)
	if len(items) > 1 {
		subject = fmt.Sprintf("%d new items on %s", len(items), a.cfg.SiteTitle)
	}
	rows, err := a.db.Query(`SELECT email, unsub_token FROM users WHERE notify_new=1`)
	if err != nil {
		log.Printf("digest: %v", err)
		return
	}
	defer rows.Close()
	sent := 0
	for rows.Next() {
		var email, unsub string
		rows.Scan(&email, &unsub)
		if a.cfg.AdminEmails[email] {
			continue
		}
		body := "New things are up for grabs:\n\n" + list.String() +
			"Browse everything: " + a.cfg.BaseURL + "/\n\n" +
			"Don't want these emails? " + a.cfg.BaseURL + "/unsubscribe?t=" + url.QueryEscape(unsub) + "\n"
		a.notify(email, subject, body)
		sent++
	}
	log.Printf("new-items digest: %d items to %d people", len(items), sent)
}

func (a *App) modeSummary(it *Item) string {
	switch it.Mode {
	case "lottery":
		return "Lottery — enter by " + it.LotteryEndsAt.Format("Mon Jan 2, 3:04 PM MST")
	case "pick":
		return "Request it — I'll choose"
	default:
		return "First come, first served"
	}
}
