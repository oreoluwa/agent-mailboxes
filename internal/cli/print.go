package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

func PrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func PrintInboxList(items []model.ListItem) {
	fmt.Printf("%-36s  %-10s  %-20s  %-30s  %-22s  %-8s\n", "MSG_ID", "FOLDER", "RECEIVED", "FROM", "SUBJECT", "PRIORITY")
	for _, it := range items {
		sub := ""
		if it.Subject != nil {
			sub = *it.Subject
		}
		from := it.From.Email
		if it.From.Name != nil {
			from = *it.From.Name + " <" + it.From.Email + ">"
		}
		fmt.Printf("%-36s  %-10s  %-20s  %-30s  %-22s  %-8s\n", it.MsgID, trim(it.Folder, 10), it.ReceivedAt.Format("2006-01-02 15:04"), trim(from, 30), trim(sub, 22), it.Digest.Priority)
		fmt.Printf("  digest: %s\n", trim(it.Digest.SummaryShort, 120))
		if len(it.Digest.SummaryBullets) > 0 {
			fmt.Printf("  bullets: %s\n", strings.Join(it.Digest.SummaryBullets, " | "))
		}
	}
}

func PrintMessage(m model.Message) {
	fmt.Printf("Message: %s\n", m.ID)
	fmt.Printf("From: %s\n", m.FromEmail)
	if m.Subject != nil {
		fmt.Printf("Subject: %s\n", *m.Subject)
	}
	fmt.Printf("Received: %s\n", m.InternalDate.Format("2006-01-02 15:04:05Z07:00"))
	fmt.Printf("Digest: %s\n", m.Digest.SummaryShort)
	for _, b := range m.Digest.SummaryBullets {
		fmt.Printf("- %s\n", b)
	}
	fmt.Printf("Intent=%s Priority=%s Action=%s Confidence=%.2f\n", m.Digest.Intent, m.Digest.Priority, m.Digest.RequiredAction, m.Digest.Confidence)
	if len(m.Attachments) > 0 {
		fmt.Println("Attachments:")
		for _, a := range m.Attachments {
			fmt.Printf("- %s (%s, %d bytes)\n", a.Filename, a.MIME, a.SizeBytes)
		}
	}
}

func trim(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
