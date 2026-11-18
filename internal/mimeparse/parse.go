package mimeparse

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/oreoluwa/agent-mailboxes/internal/model"
)

var wsRe = regexp.MustCompile(`\s+`)

func ParseRawEmail(raw []byte, uid uint32, internalDate time.Time) (model.ParsedEmail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return model.ParsedEmail{}, fmt.Errorf("read message: %w", err)
	}
	headers := msg.Header
	rawHeaders := flattenHeaders(headers)
	fromAddr, fromName := parseFirstAddress(headers.Get("From"))
	var sentDate *time.Time
	if d, err := mail.ParseDate(headers.Get("Date")); err == nil {
		sentDate = &d
	}
	subj := nullableString(headers.Get("Subject"))
	inReplyTo := nullableString(headers.Get("In-Reply-To"))
	references := nullableString(headers.Get("References"))

	bodyBytes, err := io.ReadAll(msg.Body)
	if err != nil {
		return model.ParsedEmail{}, fmt.Errorf("read body: %w", err)
	}

	textPlain, textHTML, attachments := parseBody(headers.Get("Content-Type"), headers.Get("Content-Transfer-Encoding"), bodyBytes)
	extracted := extractText(textPlain, textHTML)
	snippet := nullableString(snippetFrom(extracted, 200))
	recipients := collectRecipients(headers)

	return model.ParsedEmail{
		IMAPUID:       int64(uid),
		RawRFC822:     append([]byte(nil), raw...),
		RFCMessageID:  headers.Get("Message-ID"),
		InternalDate:  internalDate,
		SentDate:      sentDate,
		FromEmail:     fromAddr,
		FromName:      fromName,
		Subject:       subj,
		RawHeaders:    rawHeaders,
		InReplyTo:     inReplyTo,
		References:    references,
		Recipients:    recipients,
		TextPlain:     nullableString(textPlain),
		TextHTML:      nullableString(textHTML),
		ExtractedText: extracted,
		Snippet:       snippet,
		Attachments:   attachments,
		HasAttachment: len(attachments) > 0,
	}, nil
}

func parseBody(contentType, transferEncoding string, body []byte) (string, string, []model.Attachment) {
	body = decodeTransferEncoding(body, transferEncoding)
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		var plain string
		var htmlBody string
		attachments := []model.Attachment{}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			partBytes, _ := io.ReadAll(p)
			partBytes = decodeTransferEncoding(partBytes, p.Header.Get("Content-Transfer-Encoding"))
			mt, ctParams, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			cd, cdParams, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
			filename := cdParams["filename"]
			if filename == "" {
				filename = ctParams["name"]
			}
			if strings.EqualFold(cd, "attachment") || filename != "" {
				attachments = append(attachments, model.Attachment{
					Filename:  filename,
					MIME:      mt,
					SizeBytes: int64(len(partBytes)),
					Content:   partBytes,
				})
				continue
			}

			if strings.HasPrefix(mt, "multipart/") {
				partPlain, partHTML, partAttachments := parseBody(p.Header.Get("Content-Type"), "", partBytes)
				if plain == "" && partPlain != "" {
					plain = partPlain
				}
				if htmlBody == "" && partHTML != "" {
					htmlBody = partHTML
				}
				attachments = append(attachments, partAttachments...)
				continue
			}

			switch {
			case strings.HasPrefix(mt, "text/plain"):
				if plain == "" {
					plain = string(partBytes)
				}
			case strings.HasPrefix(mt, "text/html"):
				if htmlBody == "" {
					htmlBody = string(partBytes)
				}
			}
		}
		return plain, htmlBody, attachments
	}
	if strings.HasPrefix(mediaType, "text/html") {
		return "", string(body), nil
	}
	return string(body), "", nil
}

func extractText(plain, htmlBody string) string {
	if strings.TrimSpace(plain) != "" {
		return normalizeWhitespace(plain)
	}
	if strings.TrimSpace(htmlBody) != "" {
		node, err := html.Parse(strings.NewReader(htmlBody))
		if err != nil {
			return normalizeWhitespace(htmlBody)
		}
		var b strings.Builder
		var f func(*html.Node)
		f = func(n *html.Node) {
			if n.Type == html.TextNode {
				b.WriteString(n.Data)
				b.WriteRune(' ')
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				f(c)
			}
		}
		f(node)
		return normalizeWhitespace(b.String())
	}
	return ""
}

func normalizeWhitespace(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

func snippetFrom(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "..."
}

func collectRecipients(h mail.Header) []model.Recipient {
	fields := []struct {
		name string
		typ  string
	}{
		{"To", "to"},
		{"Cc", "cc"},
		{"Bcc", "bcc"},
		{"Delivered-To", "to"},
		{"Envelope-To", "to"},
		{"X-Original-To", "to"},
	}
	var out []model.Recipient
	for _, field := range fields {
		for _, value := range textproto.MIMEHeader(h).Values(field.name) {
			for _, e := range parseRecipientAddresses(value) {
				out = append(out, model.Recipient{Type: field.typ, Email: e})
			}
		}
	}
	return out
}

func parseRecipientAddresses(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	addrs, err := mail.ParseAddressList(v)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		email := strings.ToLower(strings.TrimSpace(addr.Address))
		if strings.Count(email, "@") != 1 || strings.ContainsAny(email, " \t\r\n") {
			continue
		}
		parts := strings.SplitN(email, "@", 2)
		if parts[0] == "" || parts[1] == "" {
			continue
		}
		out = append(out, email)
	}
	return out
}

func extractEmails(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	addrs, err := mail.ParseAddressList(v)
	if err == nil {
		out := make([]string, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, strings.ToLower(strings.TrimSpace(a.Address)))
		}
		return out
	}
	return regexEmails(v)
}

func regexEmails(v string) []string {
	re := regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	return re.FindAllString(strings.ToLower(v), -1)
}

func flattenHeaders(h mail.Header) string {
	var b strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func parseFirstAddress(v string) (string, *string) {
	addr, err := mail.ParseAddress(v)
	if err != nil {
		emails := regexEmails(v)
		if len(emails) > 0 {
			return emails[0], nil
		}
		return "", nil
	}
	name := nullableString(addr.Name)
	return strings.ToLower(addr.Address), name
}

func nullableString(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	t := strings.TrimSpace(v)
	return &t
}

func decodeTransferEncoding(part []byte, enc string) []byte {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(part)))
		if err != nil {
			return part
		}
		return decoded
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(part)))
		if err != nil {
			return part
		}
		return decoded
	default:
		return part
	}
}
