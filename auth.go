package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

const (
	sessionCookie = "sid"
	sessionTTL    = 90 * 24 * time.Hour
	loginTTL      = 30 * time.Minute
	loginCooldown = time.Minute
)

type ctxKey struct{}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func (a *App) isAdmin(u *User) bool { return u != nil && a.cfg.AdminEmails[normEmail(u.Email)] }

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(ctxKey{}).(*User)
	return u
}

func (a *App) userFromCookie(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var u User
	err = a.db.QueryRow(`SELECT u.id, u.email, u.name, u.notify_new FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.hash=? AND s.expires_at>?`, hashToken(c.Value), time.Now().Unix()).Scan(&u.ID, &u.Email, &u.Name, &u.NotifyNew)
	if err != nil {
		return nil
	}
	return &u
}

func (a *App) requireUser(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := a.userFromCookie(r)
		if u == nil {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		// Ask for a display name before anything else so claims show who's who.
		if u.Name == "" && r.URL.Path != "/profile" && r.Method == http.MethodGet {
			http.Redirect(w, r, "/profile?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func (a *App) requireAdmin(h http.HandlerFunc) http.Handler {
	return a.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !a.isAdmin(currentUser(r)) {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	})
}

// safeNext only allows same-site relative redirects.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// emailAllowed: admins always; everyone else only if they're on the invite
// list, or if the invite list is empty (open to anyone with the link).
func (a *App) emailAllowed(email string) bool {
	if a.cfg.AdminEmails[email] {
		return true
	}
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM allowed_emails`).Scan(&n)
	if n == 0 {
		return true
	}
	var ok int
	a.db.QueryRow(`SELECT 1 FROM allowed_emails WHERE email=?`, email).Scan(&ok)
	return ok == 1
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.userFromCookie(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	a.render(w, r, "login.html", "Sign in", map[string]string{"Next": r.URL.Query().Get("next")})
}

func (a *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	addr, err := mail.ParseAddress(r.FormValue("email"))
	next := safeNext(r.FormValue("next"))
	if err != nil || strings.ContainsAny(addr.Address, "\r\n") {
		a.setFlash(w, "That doesn't look like an email address.")
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	email := normEmail(addr.Address)
	now := time.Now()
	if a.emailAllowed(email) {
		var recent int
		a.db.QueryRow(`SELECT COUNT(*) FROM login_tokens WHERE email=? AND created_at>?`,
			email, now.Add(-loginCooldown).Unix()).Scan(&recent)
		if recent == 0 {
			token := rand.Text()
			if _, err := a.db.Exec(`INSERT INTO login_tokens(hash, email, created_at, expires_at) VALUES(?,?,?,?)`,
				hashToken(token), email, now.Unix(), now.Add(loginTTL).Unix()); err != nil {
				a.serverError(w, err)
				return
			}
			link := a.cfg.BaseURL + "/auth?token=" + url.QueryEscape(token) + "&next=" + url.QueryEscape(next)
			body := "Tap this link to sign in to " + a.cfg.SiteTitle + ":\n\n" + link +
				"\n\nThe link works once and expires in 30 minutes. If you didn't ask for this, ignore this email.\n"
			if err := a.sendMail(email, "Your sign-in link for "+a.cfg.SiteTitle, body); err != nil {
				log.Printf("login mail to %s failed: %v", email, err)
				a.setFlash(w, "Sorry, the email couldn't be sent. Try again in a minute.")
				http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
				return
			}
		}
	} else {
		log.Printf("login attempt from email not on the invite list: %s", email)
	}
	// Same response either way so the page doesn't reveal who's invited.
	http.Redirect(w, r, "/login/sent?email="+url.QueryEscape(email), http.StatusSeeOther)
}

func (a *App) loginSent(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "login_sent.html", "Check your email", map[string]string{"Email": r.URL.Query().Get("email")})
}

// GET shows a button instead of signing in directly: email security scanners
// prefetch links and would otherwise burn the one-time token.
func (a *App) authConfirmPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "auth.html", "Sign in", map[string]string{
		"Token": r.URL.Query().Get("token"), "Next": r.URL.Query().Get("next"),
	})
}

func (a *App) authConfirm(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	now := time.Now().Unix()
	tx, err := a.db.Begin()
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer tx.Rollback()
	var email string
	err = tx.QueryRow(`SELECT email FROM login_tokens WHERE hash=? AND used_at IS NULL AND expires_at>?`,
		hashToken(token), now).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		a.setFlash(w, "That sign-in link has expired or was already used. Request a new one.")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	tx.Exec(`UPDATE login_tokens SET used_at=? WHERE hash=?`, now, hashToken(token))
	tx.Exec(`INSERT INTO users(email, unsub_token, created_at) VALUES(?,?,?) ON CONFLICT(email) DO NOTHING`,
		email, rand.Text(), now)
	var userID int64
	if err := tx.QueryRow(`SELECT id FROM users WHERE email=?`, email).Scan(&userID); err != nil {
		a.serverError(w, err)
		return
	}
	sid := rand.Text()
	if _, err := tx.Exec(`INSERT INTO sessions(hash, user_id, expires_at) VALUES(?,?,?)`,
		hashToken(sid), userID, time.Now().Add(sessionTTL).Unix()); err != nil {
		a.serverError(w, err)
		return
	}
	tx.Exec(`DELETE FROM sessions WHERE expires_at<?`, now)
	tx.Exec(`DELETE FROM login_tokens WHERE expires_at<?`, now-86400)
	if err := tx.Commit(); err != nil {
		a.serverError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sid, Path: "/", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE hash=?`, hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) profilePage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "profile.html", "Your profile", map[string]any{"Next": r.URL.Query().Get("next")})
}

func (a *App) profileSave(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 60 {
		a.setFlash(w, "Please enter a name (up to 60 characters) so we know who's claiming what.")
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	notify := r.FormValue("notify_new") == "1"
	if _, err := a.db.Exec(`UPDATE users SET name=?, notify_new=? WHERE id=?`, name, notify, u.ID); err != nil {
		a.serverError(w, err)
		return
	}
	a.setFlash(w, "Saved.")
	next := r.FormValue("next")
	if next == "" {
		next = "/profile"
	}
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

// unsubscribe works without signing in, from the link in notification emails.
func (a *App) unsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("t")
	if r.Method == http.MethodPost && token != "" {
		a.db.Exec(`UPDATE users SET notify_new=0 WHERE unsub_token=?`, token)
		a.render(w, r, "unsubscribed.html", "Unsubscribed", map[string]any{"Done": true})
		return
	}
	a.render(w, r, "unsubscribed.html", "Unsubscribe", map[string]any{"Token": token})
}
