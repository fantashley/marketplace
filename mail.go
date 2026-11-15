package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"crypto/rand"
)

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
}

type MailConfig struct {
	From         string // "Name <addr@example.com>"
	ResendAPIKey string // takes precedence over SMTP when set
	SMTP         SMTPConfig
}

func (m MailConfig) enabled() bool { return m.ResendAPIKey != "" || m.SMTP.Host != "" }

// deliver hands one plain-text email to the configured transport, or logs it
// when there is none. Callers go through sendMail (throttled) or notify (queued).
func (a *App) deliver(to, subject, body string) error {
	m := a.cfg.Mail
	switch {
	case m.ResendAPIKey != "":
		return sendResend(m, to, subject, body)
	case m.SMTP.Host != "":
		return sendSMTP(m, to, subject, body)
	default:
		log.Printf("[mail not configured] To: %s | Subject: %s\n%s", to, subject, body)
		return nil
	}
}

func sendResend(m MailConfig, to, subject, body string) error {
	payload, _ := json.Marshal(map[string]any{
		"from": m.From, "to": []string{to}, "subject": subject, "text": body,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.resend.com/emails", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+m.ResendAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("resend: %s: %s", resp.Status, msg)
	}
	return nil
}

func sendSMTP(m MailConfig, to, subject, body string) error {
	c := m.SMTP
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return fmt.Errorf("bad MAIL_FROM: %w", err)
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	if c.Port == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: c.Host})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()
	if c.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
				return err
			}
		}
	}
	if c.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err := client.Mail(from.Address); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\n",
		m.From, to, mime.QEncoding.Encode("utf-8", subject), time.Now().Format(time.RFC1123Z),
		strings.ToLower(rand.Text()), domainOf(from.Address))
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	if _, err := io.WriteString(w, msg.String()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}
