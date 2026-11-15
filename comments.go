package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const maxCommentLen = 1000

// postComment adds a question or comment to an item. Comments are visible to
// everyone who can see the item, so a question only needs answering once.
func (a *App) postComment(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	it, err := a.getItem(u.ID, id)
	if err != nil || (it.Status == "hidden" && !a.isAdmin(u)) {
		http.NotFound(w, r)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	if body == "" {
		http.Redirect(w, r, itemURL(id)+"#comments", http.StatusSeeOther)
		return
	}
	if utf8.RuneCountInString(body) > maxCommentLen {
		a.setFlash(w, fmt.Sprintf("Comments can be up to %d characters.", maxCommentLen))
		http.Redirect(w, r, itemURL(id)+"#comments", http.StatusSeeOther)
		return
	}
	res, err := a.db.Exec(`INSERT INTO comments(item_id, user_id, body, created_at) VALUES(?,?,?,?)`,
		id, u.ID, body, time.Now().Unix())
	if err != nil {
		a.serverError(w, err)
		return
	}
	cid, _ := res.LastInsertId()
	a.notifyComment(it, u, body)
	http.Redirect(w, r, fmt.Sprintf("%s#c%d", itemURL(id), cid), http.StatusSeeOther)
}

// notifyComment emails the admins about comments from friends, and everyone
// who already commented on the item (if they want reply emails) about new ones.
func (a *App) notifyComment(it *Item, author *User, body string) {
	link := fmt.Sprintf("%s%s#comments", a.cfg.BaseURL, itemURL(it.ID))
	subject := fmt.Sprintf("%s commented on %s", author.Display(), it.Title)
	msg := fmt.Sprintf("%s wrote on %q:\n\n%s\n\nReply: %s\n", author.Display(), it.Title, body, link)

	sent := map[string]bool{normEmail(author.Email): true}
	if !a.isAdmin(author) {
		for email := range a.cfg.AdminEmails {
			if !sent[email] {
				sent[email] = true
				a.notify(email, subject, msg)
			}
		}
	}
	// Collect first: notify writes to the database, which has one connection.
	rows, err := a.db.Query(`SELECT DISTINCT u.email FROM comments c JOIN users u ON u.id=c.user_id
		WHERE c.item_id=? AND u.notify_replies=1`, it.ID)
	if err != nil {
		return
	}
	var participants []string
	for rows.Next() {
		var email string
		rows.Scan(&email)
		participants = append(participants, normEmail(email))
	}
	rows.Close()
	for _, email := range participants {
		if sent[email] || !a.emailAllowed(email) {
			continue
		}
		sent[email] = true
		a.notify(email, subject, msg+"\nYou're getting this because you commented on this item. "+
			"Turn off reply emails: "+a.cfg.BaseURL+"/profile\n")
	}
}

// deleteComment lets authors remove their own comments and admins remove any.
func (a *App) deleteComment(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var itemID, authorID int64
	if err := a.db.QueryRow(`SELECT item_id, user_id FROM comments WHERE id=?`, pathID(r)).Scan(&itemID, &authorID); err != nil {
		http.NotFound(w, r)
		return
	}
	if authorID != u.ID && !a.isAdmin(u) {
		http.NotFound(w, r)
		return
	}
	if _, err := a.db.Exec(`DELETE FROM comments WHERE id=?`, pathID(r)); err != nil {
		a.serverError(w, err)
		return
	}
	a.setFlash(w, "Comment deleted.")
	http.Redirect(w, r, itemURL(itemID)+"#comments", http.StatusSeeOther)
}
