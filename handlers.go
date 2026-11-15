package main

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func itemURL(id int64) string { return fmt.Sprintf("/items/%d", id) }

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	a.settleLotteries()
	u := currentUser(r)
	where := "i.status!='hidden'"
	if a.isAdmin(u) {
		where = ""
	}
	items, err := a.listItems(u.ID, where)
	if err != nil {
		a.serverError(w, err)
		return
	}
	var open, claimed, hidden []*Item
	for _, it := range items {
		switch it.Status {
		case "open":
			open = append(open, it)
		case "claimed":
			claimed = append(claimed, it)
		default:
			hidden = append(hidden, it)
		}
	}
	a.render(w, r, "home.html", "", map[string]any{"Open": open, "Claimed": claimed, "Hidden": hidden})
}

func (a *App) minePage(w http.ResponseWriter, r *http.Request) {
	a.settleLotteries()
	u := currentUser(r)
	won, err := a.listItems(u.ID, "i.status='claimed' AND i.winner_id=?", u.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	pending, err := a.listItems(u.ID, "i.status='open' AND EXISTS(SELECT 1 FROM requests r WHERE r.item_id=i.id AND r.user_id=?)", u.ID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "mine.html", "My stuff", map[string]any{"Won": won, "Pending": pending})
}

func (a *App) itemPage(w http.ResponseWriter, r *http.Request) {
	a.settleLotteries()
	u := currentUser(r)
	it, err := a.getItem(u.ID, pathID(r))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && it.Status == "hidden" && !a.isAdmin(u)) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	a.render(w, r, "item.html", it.Title, map[string]any{"Item": it, "IsWinner": it.WinnerID == u.ID})
}

func (a *App) claim(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	res, err := a.db.Exec(`UPDATE items SET status='claimed', winner_id=?, claimed_at=? WHERE id=? AND status='open' AND mode='fcfs'`,
		u.ID, time.Now().Unix(), id)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.setFlash(w, "Too late — someone else got it first.")
	} else {
		a.setFlash(w, "It's yours! I'll be in touch about the handoff.")
		a.notifyAdmins(u.Display()+" claimed an item", fmt.Sprintf("%s claimed item %d.\n%s%s\n", u.Display(), id, a.cfg.BaseURL, itemURL(id)))
	}
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

func (a *App) enter(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	it, err := a.getItem(u.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !(it.LotteryOpen() || (it.Mode == "pick" && it.Status == "open")) {
		a.setFlash(w, "This item isn't taking requests right now.")
		http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if len(note) > 500 {
		note = note[:500]
	}
	if _, err := a.db.Exec(`INSERT INTO requests(item_id, user_id, note, created_at) VALUES(?,?,?,?)
		ON CONFLICT(item_id, user_id) DO UPDATE SET note=excluded.note`, id, u.ID, note, time.Now().Unix()); err != nil {
		a.serverError(w, err)
		return
	}
	if it.Mode == "lottery" {
		a.setFlash(w, "You're in the lottery. You'll get an email if you win.")
	} else {
		a.setFlash(w, "Request sent.")
		if !it.Mine {
			a.notifyAdmins(u.Display()+" wants "+it.Title, fmt.Sprintf("%s requested %q.\nNote: %s\n\nPick someone: %s%s\n",
				u.Display(), it.Title, note, a.cfg.BaseURL, itemURL(id)))
		}
	}
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

func (a *App) leave(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	a.db.Exec(`DELETE FROM requests WHERE item_id=? AND user_id=? AND item_id IN (SELECT id FROM items WHERE status='open')`, id, u.ID)
	a.setFlash(w, "Request withdrawn.")
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

func (a *App) release(w http.ResponseWriter, r *http.Request) {
	u, id := currentUser(r), pathID(r)
	var title string
	a.db.QueryRow(`SELECT title FROM items WHERE id=?`, id).Scan(&title)
	if err := a.unclaim(id, u.ID); errors.Is(err, sql.ErrNoRows) {
		// Not (or no longer) theirs, e.g. a double tap after it was passed on.
		http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	a.setFlash(w, "Thanks — it's been passed along.")
	a.notifyAdmins(u.Display()+" gave back "+title, fmt.Sprintf("%s gave back %q.\n%s%s\n", u.Display(), title, a.cfg.BaseURL, itemURL(id)))
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

func (a *App) photo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeFile(w, r, filepath.Join(a.photoDir, name))
}

// ---- admin ----

type itemForm struct {
	Title, Description, Mode, Status, Handoff, HandoffNote, AvailableFrom string
	EndsAt                                                                time.Time // zero = unchanged
	LotterySecs                                                           int64     // window length chosen with EndsAt
}

func parseItemForm(r *http.Request) (itemForm, error) {
	f := itemForm{
		Title:         strings.TrimSpace(r.FormValue("title")),
		Description:   strings.TrimSpace(r.FormValue("description")),
		Mode:          r.FormValue("mode"),
		Status:        r.FormValue("status"),
		Handoff:       r.FormValue("handoff"),
		HandoffNote:   strings.TrimSpace(r.FormValue("handoff_note")),
		AvailableFrom: strings.TrimSpace(r.FormValue("available_from")),
	}
	if f.AvailableFrom != "" {
		if _, err := time.Parse(dateLayout, f.AvailableFrom); err != nil {
			return f, errors.New("Available-from date should look like 2026-12-20.")
		}
	}
	if f.Title == "" {
		return f, errors.New("Title is required.")
	}
	if f.Mode != "fcfs" && f.Mode != "lottery" && f.Mode != "pick" {
		return f, errors.New("Pick how people claim it.")
	}
	if f.Status != "hidden" {
		f.Status = "open"
	}
	if _, ok := handoffLabels[f.Handoff]; !ok {
		f.Handoff = "flexible"
	}
	if h := strings.TrimSpace(r.FormValue("lottery_hours")); h != "" {
		hours, err := strconv.ParseFloat(h, 64)
		if err != nil || hours <= 0 || hours > 24*30 {
			return f, errors.New("Lottery length must be between 0 and 720 hours.")
		}
		f.LotterySecs = int64(hours * 3600)
		f.EndsAt = time.Now().Add(time.Duration(f.LotterySecs) * time.Second)
	}
	return f, nil
}

const maxUpload = 200 << 20

func (a *App) newItemPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "item_form.html", "Post an item", map[string]any{
		"Item": &Item{Mode: "lottery", Status: "open", Handoff: "flexible"},
	})
}

func (a *App) createItem(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		a.setFlash(w, "Upload failed (too large?).")
		http.Redirect(w, r, "/admin/new", http.StatusSeeOther)
		return
	}
	f, err := parseItemForm(r)
	if err == nil && f.Mode == "lottery" && f.EndsAt.IsZero() {
		err = errors.New("Set how long the lottery runs.")
	}
	if err != nil {
		a.setFlash(w, err.Error())
		http.Redirect(w, r, "/admin/new", http.StatusSeeOther)
		return
	}
	now := time.Now().Unix()
	var ends, published any
	if f.Mode == "lottery" {
		ends = f.EndsAt.Unix()
	}
	if f.Status == "open" {
		published = now
	}
	res, err := a.db.Exec(`INSERT INTO items(title, description, mode, lottery_ends_at, lottery_secs, status, handoff, handoff_note, available_from, published_at, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, f.Title, f.Description, f.Mode, ends, f.LotterySecs, f.Status, f.Handoff, f.HandoffNote, f.AvailableFrom, published, now)
	if err != nil {
		a.serverError(w, err)
		return
	}
	id, _ := res.LastInsertId()
	if err := a.savePhotos(r, id); err != nil {
		a.setFlash(w, "Item saved, but: "+err.Error())
	} else if f.Status == "open" {
		a.setFlash(w, fmt.Sprintf("Posted! Friends get one email once you've stopped posting for %s.", a.cfg.NewItemsDelay))
	} else {
		a.setFlash(w, "Saved as hidden.")
	}
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

func (a *App) editItemPage(w http.ResponseWriter, r *http.Request) {
	a.settleLotteries()
	it, err := a.getItem(0, pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.render(w, r, "item_form.html", "Edit "+it.Title, map[string]any{"Item": it})
}

func (a *App) updateItem(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	editURL := fmt.Sprintf("/admin/items/%d/edit", id)
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		a.setFlash(w, "Upload failed (too large?).")
		http.Redirect(w, r, editURL, http.StatusSeeOther)
		return
	}
	f, err := parseItemForm(r)
	if err != nil {
		a.setFlash(w, err.Error())
		http.Redirect(w, r, editURL, http.StatusSeeOther)
		return
	}
	var curEnds, curSecs int64
	var curStatus string
	var notified bool
	if err := a.db.QueryRow(`SELECT COALESCE(lottery_ends_at,0), lottery_secs, status, notified FROM items WHERE id=?`, id).
		Scan(&curEnds, &curSecs, &curStatus, &notified); err != nil {
		http.NotFound(w, r)
		return
	}
	now := time.Now()
	status := f.Status
	if curStatus == "claimed" { // a claimed item stays claimed; the checkbox isn't shown
		status = "claimed"
	}
	publishing := curStatus == "hidden" && status == "open"

	var ends any
	secs := curSecs
	if f.Mode == "lottery" {
		switch {
		case !f.EndsAt.IsZero():
			ends, secs = f.EndsAt.Unix(), f.LotterySecs
		case publishing && curSecs > 0:
			// Nobody could enter while it was a draft, so the window starts now.
			ends = now.Add(time.Duration(curSecs) * time.Second).Unix()
		default:
			ends = curEnds
		}
		if status == "open" && ends.(int64) <= now.Unix() {
			a.setFlash(w, "That lottery's deadline has passed. Set how many hours it should run from now.")
			http.Redirect(w, r, editURL, http.StatusSeeOther)
			return
		}
	}
	publishedAt := sql.NullInt64{}
	if publishing && !notified {
		// (Re)publishing restarts the new-items quiet period; an item that was
		// already announced before being hidden isn't announced twice.
		publishedAt = sql.NullInt64{Int64: now.Unix(), Valid: true}
	}
	_, err = a.db.Exec(`UPDATE items SET title=?, description=?, mode=?, lottery_ends_at=?, lottery_secs=?, handoff=?, handoff_note=?, available_from=?,
		status=?, published_at=CASE WHEN ? IS NOT NULL THEN ? ELSE published_at END
		WHERE id=?`, f.Title, f.Description, f.Mode, ends, secs, f.Handoff, f.HandoffNote, f.AvailableFrom,
		status, publishedAt, publishedAt, id)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if err := a.savePhotos(r, id); err != nil {
		a.setFlash(w, "Saved, but: "+err.Error())
	} else {
		a.setFlash(w, "Saved.")
	}
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

var imageExt = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif"}

func (a *App) savePhotos(r *http.Request, itemID int64) error {
	if r.MultipartForm == nil {
		return nil
	}
	var pos int
	a.db.QueryRow(`SELECT COALESCE(MAX(position),0) FROM photos WHERE item_id=?`, itemID).Scan(&pos)
	var skipped []string
	for _, fh := range r.MultipartForm.File["photos"] {
		if err := func() error {
			src, err := fh.Open()
			if err != nil {
				return err
			}
			defer src.Close()
			head := make([]byte, 512)
			n, _ := io.ReadFull(src, head)
			ext, ok := imageExt[http.DetectContentType(head[:n])]
			if !ok {
				return errors.New("unsupported format")
			}
			if _, err := src.Seek(0, io.SeekStart); err != nil {
				return err
			}
			name := strings.ToLower(rand.Text()) + ext
			dst, err := os.Create(filepath.Join(a.photoDir, name))
			if err != nil {
				return err
			}
			if _, err := io.Copy(dst, src); err != nil {
				dst.Close()
				os.Remove(dst.Name())
				return err
			}
			if err := dst.Close(); err != nil {
				return err
			}
			pos++
			_, err = a.db.Exec(`INSERT INTO photos(item_id, filename, position) VALUES(?,?,?)`, itemID, name, pos)
			return err
		}(); err != nil {
			log.Printf("photo %q: %v", fh.Filename, err)
			skipped = append(skipped, fh.Filename)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("couldn't save %s (use JPEG, PNG, WebP or GIF)", strings.Join(skipped, ", "))
	}
	return nil
}

func (a *App) deleteItem(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	rows, err := a.db.Query(`SELECT filename FROM photos WHERE item_id=?`, id)
	if err != nil {
		a.serverError(w, err)
		return
	}
	var files []string
	for rows.Next() {
		var f string
		rows.Scan(&f)
		files = append(files, f)
	}
	rows.Close()
	if _, err := a.db.Exec(`DELETE FROM items WHERE id=?`, id); err != nil {
		a.serverError(w, err)
		return
	}
	for _, f := range files {
		os.Remove(filepath.Join(a.photoDir, f))
	}
	a.setFlash(w, "Item deleted.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) award(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	userID, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	res, err := a.db.Exec(`UPDATE items SET status='claimed', winner_id=?, claimed_at=?, picked_up=0 WHERE id=? AND status!='claimed'
		AND EXISTS(SELECT 1 FROM users WHERE id=?)`, userID, time.Now().Unix(), id, userID)
	if err != nil {
		a.serverError(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		a.notifyWinner(id, userID, "Good news!")
		a.setFlash(w, "Given away. They've been emailed.")
	}
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

func (a *App) reopen(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	// The form carries the winner shown on the page, so a double submit or a
	// stale tab can't take the item from whoever won the redraw.
	winner, _ := strconv.ParseInt(r.FormValue("winner_id"), 10, 64)
	if err := a.unclaim(id, winner); errors.Is(err, sql.ErrNoRows) {
		a.setFlash(w, "It had already changed hands, so nothing was reopened.")
		http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
		return
	} else if err != nil {
		a.serverError(w, err)
		return
	}
	a.setFlash(w, "Reopened. (Lotteries redraw right away among the other entrants.)")
	http.Redirect(w, r, itemURL(id), http.StatusSeeOther)
}

func (a *App) togglePickedUp(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	a.db.Exec(`UPDATE items SET picked_up=1-picked_up WHERE id=?`, id)
	back := safeNext(r.FormValue("back"))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (a *App) deletePhoto(w http.ResponseWriter, r *http.Request) {
	var itemID int64
	var name string
	if err := a.db.QueryRow(`SELECT item_id, filename FROM photos WHERE id=?`, pathID(r)).Scan(&itemID, &name); err != nil {
		http.NotFound(w, r)
		return
	}
	a.db.Exec(`DELETE FROM photos WHERE id=?`, pathID(r))
	os.Remove(filepath.Join(a.photoDir, name))
	http.Redirect(w, r, fmt.Sprintf("/admin/items/%d/edit", itemID), http.StatusSeeOther)
}

type handoffGroup struct {
	Name, Email string
	Items       []*Item
	Remaining   int
}

func (a *App) handoffsPage(w http.ResponseWriter, r *http.Request) {
	items, err := a.listItems(0, "i.status='claimed'")
	if err != nil {
		a.serverError(w, err)
		return
	}
	groups := map[int64]*handoffGroup{}
	var list []*handoffGroup
	for _, it := range items {
		g := groups[it.WinnerID]
		if g == nil {
			g = &handoffGroup{Name: it.WinnerDisplay(), Email: it.WinnerEmail}
			groups[it.WinnerID] = g
			list = append(list, g)
		}
		g.Items = append(g.Items, it)
		if !it.PickedUp {
			g.Remaining++
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if (list[i].Remaining == 0) != (list[j].Remaining == 0) {
			return list[i].Remaining > 0
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	a.render(w, r, "handoffs.html", "Handoffs", map[string]any{"Groups": list})
}

type person struct {
	Name, Email string
	Won         int
	NotifyNew   bool
}

func (a *App) peoplePage(w http.ResponseWriter, r *http.Request) {
	var allowed []string
	rows, err := a.db.Query(`SELECT email FROM allowed_emails ORDER BY email`)
	if err != nil {
		a.serverError(w, err)
		return
	}
	for rows.Next() {
		var e string
		rows.Scan(&e)
		allowed = append(allowed, e)
	}
	rows.Close()
	var people []person
	rows, err = a.db.Query(`SELECT u.name, u.email, u.notify_new, (SELECT COUNT(*) FROM items i WHERE i.winner_id=u.id AND i.status='claimed')
		FROM users u ORDER BY lower(u.name), u.email`)
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var p person
		rows.Scan(&p.Name, &p.Email, &p.NotifyNew, &p.Won)
		p.Name = displayName(p.Name, p.Email)
		people = append(people, p)
	}
	a.render(w, r, "people.html", "People", map[string]any{"Allowed": strings.Join(allowed, "\n"), "People": people, "OpenSignup": a.cfg.OpenSignup})
}

func (a *App) peopleSave(w http.ResponseWriter, r *http.Request) {
	tx, err := a.db.Begin()
	if err != nil {
		a.serverError(w, err)
		return
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM allowed_emails`)
	for _, e := range strings.FieldsFunc(r.FormValue("allowed"), func(c rune) bool { return c == '\n' || c == ',' || c == ' ' || c == '\r' || c == '\t' }) {
		if e = normEmail(e); strings.Contains(e, "@") {
			tx.Exec(`INSERT OR IGNORE INTO allowed_emails(email) VALUES(?)`, e)
		}
	}
	if err := tx.Commit(); err != nil {
		a.serverError(w, err)
		return
	}
	a.setFlash(w, "Invite list saved.")
	http.Redirect(w, r, "/admin/people", http.StatusSeeOther)
}
