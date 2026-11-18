package smtpclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Host     string
	Port     int
	TLS      bool
	Username string
	Password string
}

func (c Client) ValidateCredentials(ctx context.Context) error {
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	dialer := &net.Dialer{}
	var conn net.Conn
	var err error
	if c.TLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: c.Host}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return validationContextError(ctx, err)
	}
	defer cl.Close()
	if !c.TLS {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
				return validationContextError(ctx, err)
			}
		}
	}
	if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
		return validationContextError(ctx, err)
	}
	err = cl.Noop()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func validationContextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return err
}

func (c Client) Send(from string, to []string, subject string, body string, inReplyTo *string, references *string) error {
	if len(to) == 0 {
		return fmt.Errorf("no recipients")
	}
	headers := []string{
		fmt.Sprintf("From: %s", from),
		fmt.Sprintf("To: %s", strings.Join(to, ", ")),
		fmt.Sprintf("Subject: %s", subject),
		fmt.Sprintf("Date: %s", time.Now().Format(time.RFC1123Z)),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}
	if inReplyTo != nil {
		headers = append(headers, "In-Reply-To: "+*inReplyTo)
	}
	if references != nil {
		headers = append(headers, "References: "+*references)
	}
	msg := strings.Join(headers, "\r\n") + "\r\n\r\n" + body

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	auth := smtp.PlainAuth("", c.Username, c.Password, c.Host)
	if c.TLS {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
		if err != nil {
			return err
		}
		cl, err := smtp.NewClient(conn, c.Host)
		if err != nil {
			return err
		}
		defer cl.Quit()
		if err := cl.Auth(auth); err != nil {
			return err
		}
		if err := cl.Mail(from); err != nil {
			return err
		}
		for _, r := range to {
			if err := cl.Rcpt(r); err != nil {
				return err
			}
		}
		w, err := cl.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(msg)); err != nil {
			return err
		}
		return w.Close()
	}
	return smtp.SendMail(addr, auth, from, to, []byte(msg))
}

func (c Client) SendRaw(from string, to []string, raw []byte) error {
	if len(to) == 0 {
		return fmt.Errorf("no recipients")
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	auth := smtp.PlainAuth("", c.Username, c.Password, c.Host)
	if c.TLS {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
		if err != nil {
			return err
		}
		cl, err := smtp.NewClient(conn, c.Host)
		if err != nil {
			return err
		}
		defer cl.Quit()
		if err := cl.Auth(auth); err != nil {
			return err
		}
		if err := cl.Mail(from); err != nil {
			return err
		}
		for _, r := range to {
			if err := cl.Rcpt(r); err != nil {
				return err
			}
		}
		w, err := cl.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(raw); err != nil {
			return err
		}
		return w.Close()
	}

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return err
	}
	defer cl.Quit()
	if ok, _ := cl.Extension("STARTTLS"); ok {
		if err := cl.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
			return err
		}
	}
	if err := cl.Auth(auth); err != nil {
		return err
	}
	if err := cl.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := cl.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, strings.NewReader(string(raw))); err != nil {
		return err
	}
	return w.Close()
}
