package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"time"
)

type Page struct {
	Title   string
	Site    string
	User    *User
	IsAdmin bool
	Flash   string
	Path    string
	Data    any
}

func humanUntil(t time.Time) string {
	d := time.Until(t)
	switch {
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func (a *App) loadTemplates() error {
	funcs := template.FuncMap{
		"iso":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"fmt":   func(t time.Time) string { return t.Format("Mon Jan 2, 3:04 PM MST") },
		"until": humanUntil,
	}
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	a.tmpl = map[string]*template.Template{}
	for _, p := range pages {
		name := p[len("templates/"):]
		if name == "layout.html" || name == "partials.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", p)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		a.tmpl[name] = t
	}
	return nil
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	u := currentUser(r)
	p := Page{Title: title, Site: a.cfg.SiteTitle, User: u, IsAdmin: a.isAdmin(u), Flash: a.takeFlash(w, r), Path: r.URL.Path, Data: data}
	var buf bytes.Buffer
	if err := a.tmpl[name].Execute(&buf, p); err != nil {
		a.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

func (a *App) setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{Name: "flash", Value: url.QueryEscape(msg), Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
}

func (a *App) takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie("flash")
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: "flash", Value: "", Path: "/", MaxAge: -1})
	msg, _ := url.QueryUnescape(c.Value)
	return msg
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "Something went wrong. Try again?", http.StatusInternalServerError)
}
