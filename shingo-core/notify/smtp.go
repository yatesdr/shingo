package notify

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
)

func PlainSend(addr, user, password, from string, to []string, subject, body string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, hostOnly(addr))
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if user != "" {
		if err = client.Auth(smtp.PlainAuth("", user, password, hostOnly(addr))); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	msg := formatMessage(from, to, subject, body)
	if err = client.Mail(from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, r := range to {
		if err = client.Rcpt(r); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", r, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err = fmt.Fprint(w, msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return client.Quit()
}

func TLSSend(addr, user, password, from string, to []string, subject, body string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, hostOnly(addr))
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	server := &tls.Config{ServerName: hostOnly(addr)}
	if err = client.StartTLS(server); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}

	if user != "" {
		if err = client.Auth(smtp.PlainAuth("", user, password, hostOnly(addr))); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	msg := formatMessage(from, to, subject, body)
	if err = client.Mail(from); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, r := range to {
		if err = client.Rcpt(r); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", r, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err = fmt.Fprint(w, msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return client.Quit()
}

func FormatMessage(from string, to []string, subject, body string) string {
	return formatMessage(from, to, subject, body)
}

func formatMessage(from string, to []string, subject, body string) string {
	rfcFrom := from
	if _, err := mail.ParseAddress(from); err != nil {
		rfcFrom = fmt.Sprintf("<%s>", from)
	}
	msg := fmt.Sprintf("From: %s\r\n", rfcFrom)
	msg += fmt.Sprintf("To: %s\r\n", to[0])
	if len(to) > 1 {
		for _, r := range to[1:] {
			msg += fmt.Sprintf("To: %s\r\n", r)
		}
	}
	msg += fmt.Sprintf("Subject: %s\r\n", subject)
	msg += "MIME-Version: 1.0\r\n"
	msg += "Content-Type: text/plain; charset=\"utf-8\"\r\n"
	msg += "\r\n"
	msg += body
	return msg
}

func hostOnly(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}
