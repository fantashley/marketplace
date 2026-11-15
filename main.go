package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Config struct {
	Addr        string
	BaseURL     string
	DataDir     string
	SiteTitle   string
	AdminEmails map[string]bool
	// OpenSignup lets anyone with an inbox sign in; otherwise only admins and
	// emails on the invite list (Admin → People) can.
	OpenSignup bool
	Mail       MailConfig
	// New-item emails go out once nothing new has been posted for this long,
	// so posting ten things in a row sends one email.
	NewItemsDelay time.Duration
}

type App struct {
	cfg      Config
	db       *sql.DB
	tmpl     map[string]*template.Template
	photoDir string
	secure   bool
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// secret reads KEY, or the contents of the file named by KEY_FILE (for
// systemd credentials / sops), so secrets never need to live in the environment.
func secret(key string) string {
	if path := env(key+"_FILE", ""); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			log.Fatalf("reading %s_FILE: %v", key, err)
		}
		return strings.TrimSpace(string(b))
	}
	return env(key, "")
}

func loadConfig() Config {
	port, _ := strconv.Atoi(env("SMTP_PORT", "587"))
	admins := map[string]bool{}
	for _, e := range strings.Split(env("ADMIN_EMAILS", ""), ",") {
		if e = normEmail(e); e != "" {
			admins[e] = true
		}
	}
	delay, err := time.ParseDuration(env("NEW_ITEMS_DELAY", "10m"))
	if err != nil {
		log.Fatalf("NEW_ITEMS_DELAY: %v", err)
	}
	openSignup, err := strconv.ParseBool(env("OPEN_SIGNUP", "false"))
	if err != nil {
		log.Fatalf("OPEN_SIGNUP: %v", err)
	}
	return Config{
		NewItemsDelay: delay,
		OpenSignup:    openSignup,
		Addr:          env("ADDR", ":8080"),
		BaseURL:       strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		DataDir:       env("DATA_DIR", "./data"),
		SiteTitle:     env("SITE_TITLE", "Free Stuff"),
		AdminEmails:   admins,
		Mail: MailConfig{
			From:         env("MAIL_FROM", ""),
			ResendAPIKey: secret("RESEND_API_KEY"),
			SMTP: SMTPConfig{
				Host:     env("SMTP_HOST", ""),
				Port:     port,
				Username: env("SMTP_USERNAME", ""),
				Password: secret("SMTP_PASSWORD"),
			},
		},
	}
}

func main() {
	cfg := loadConfig()
	if len(cfg.AdminEmails) == 0 {
		log.Fatal("ADMIN_EMAILS must be set (comma-separated emails of the people posting items)")
	}
	if cfg.Mail.enabled() && cfg.Mail.From == "" {
		log.Fatal("MAIL_FROM must be set when RESEND_API_KEY or SMTP_HOST is set")
	}
	if cfg.OpenSignup {
		log.Print("OPEN_SIGNUP is on: anyone with an email address can sign in")
	}
	if !cfg.Mail.enabled() {
		log.Print("no mail transport configured: emails (including login links) are printed to this log instead of sent")
	}

	app := &App{cfg: cfg, photoDir: filepath.Join(cfg.DataDir, "photos"), secure: strings.HasPrefix(cfg.BaseURL, "https://")}
	if err := os.MkdirAll(app.photoDir, 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "marketplace.db"))
	if err != nil {
		log.Fatal(err)
	}
	app.db = db
	if err := app.loadTemplates(); err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.backgroundLoop(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("listening on %s (public URL %s)", cfg.Addr, cfg.BaseURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static), time.Hour)))

	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.loginSubmit)
	mux.HandleFunc("GET /login/sent", a.loginSent)
	mux.HandleFunc("GET /auth", a.authConfirmPage)
	mux.HandleFunc("POST /auth", a.authConfirm)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /unsubscribe", a.unsubscribe)
	mux.HandleFunc("POST /unsubscribe", a.unsubscribe)

	u := a.requireUser
	mux.Handle("GET /{$}", u(a.home))
	mux.Handle("GET /profile", u(a.profilePage))
	mux.Handle("POST /profile", u(a.profileSave))
	mux.Handle("GET /mine", u(a.minePage))
	mux.Handle("GET /items/{id}", u(a.itemPage))
	mux.Handle("POST /items/{id}/claim", u(a.claim))
	mux.Handle("POST /items/{id}/enter", u(a.enter))
	mux.Handle("POST /items/{id}/leave", u(a.leave))
	mux.Handle("POST /items/{id}/release", u(a.release))
	mux.Handle("GET /photos/{name}", u(a.photo))

	ad := a.requireAdmin
	mux.Handle("GET /admin/new", ad(a.newItemPage))
	mux.Handle("POST /admin/items", ad(a.createItem))
	mux.Handle("GET /admin/items/{id}/edit", ad(a.editItemPage))
	mux.Handle("POST /admin/items/{id}", ad(a.updateItem))
	mux.Handle("POST /admin/items/{id}/delete", ad(a.deleteItem))
	mux.Handle("POST /admin/items/{id}/award", ad(a.award))
	mux.Handle("POST /admin/items/{id}/reopen", ad(a.reopen))
	mux.Handle("POST /admin/items/{id}/pickedup", ad(a.togglePickedUp))
	mux.Handle("POST /admin/photos/{id}/delete", ad(a.deletePhoto))
	mux.Handle("GET /admin/handoffs", ad(a.handoffsPage))
	mux.Handle("GET /admin/people", ad(a.peoplePage))
	mux.Handle("POST /admin/people", ad(a.peopleSave))

	return securityHeaders(mux)
}

func cacheFor(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}
