//go:build ignore

// Command test-provider-credentials validates disposable smoke-test provider
// endpoints. It never prints credentials.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/oreoluwa/agent-mailboxes/internal/imapingest"
	"github.com/oreoluwa/agent-mailboxes/internal/smtpclient"
)

func main() {
	host := flag.String("host", "127.0.0.1", "provider host")
	imapPort := flag.Int("imap-port", 0, "implicit-TLS IMAP port")
	smtpPort := flag.Int("smtp-port", 0, "implicit-TLS SMTP port")
	username := flag.String("username", "", "test username")
	password := flag.String("password", "", "test password")
	flag.Parse()
	if *imapPort < 1 || *smtpPort < 1 || *username == "" || *password == "" {
		fail("complete test provider settings are required")
	}
	imap := imapingest.IMAPFetcher{
		Host: *host, Port: *imapPort, TLS: true,
		Username: *username, Password: *password, Folder: "INBOX",
	}
	if err := imap.ValidateCredentials(); err != nil {
		fail("disposable IMAP validation failed: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	smtp := smtpclient.Client{
		Host: *host, Port: *smtpPort, TLS: true,
		Username: *username, Password: *password,
	}
	if err := smtp.ValidateCredentials(ctx); err != nil {
		fail("disposable SMTP validation failed: " + err.Error())
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
