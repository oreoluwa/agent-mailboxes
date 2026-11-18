package imapingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

type Message struct {
	UID          uint32
	InternalDate time.Time
	Raw          []byte
}

type Fetcher interface {
	FetchSince(lastUID uint32) ([]Message, uint32, uint32, error)
}

type IdleFetcher interface {
	WaitForChange(ctx context.Context, timeout time.Duration) (bool, error)
}

type IMAPFetcher struct {
	Host     string
	Port     int
	TLS      bool
	Username string
	Password string
	Folder   string
	MaxFetch int
	// InitialSyncMaxMessages bounds the initial import per folder: zero skips
	// existing mail, -1 (or nil) imports all, and positive values keep the newest N.
	InitialSyncMaxMessages *int
}

func (f IMAPFetcher) FetchSince(lastUID uint32) ([]Message, uint32, uint32, error) {
	return f.FetchSinceFolder(f.folder(), lastUID)
}

func (f IMAPFetcher) ValidateCredentials() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return f.ValidateCredentialsContext(ctx)
}

func (f IMAPFetcher) ValidateCredentialsContext(ctx context.Context) error {
	conn, err := f.dialContext(ctx)
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
	c, err := client.New(conn)
	if err != nil {
		return validationContextError(ctx, err)
	}

	if err := c.Login(f.Username, f.Password); err != nil {
		return validationContextError(ctx, err)
	}
	_, err = c.Select(f.folder(), true)
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

func (f IMAPFetcher) FetchSinceFolder(folder string, lastUID uint32) ([]Message, uint32, uint32, error) {
	return f.fetchSinceFolder(folder, 0, lastUID, false)
}

// FetchSinceCheckpoint distinguishes an initialized empty mailbox from a new
// connection and reapplies the import policy when the server resets UIDVALIDITY.
func (f IMAPFetcher) FetchSinceCheckpoint(folder string, uidValidity, lastUID uint32) ([]Message, uint32, uint32, error) {
	return f.fetchSinceFolder(folder, uidValidity, lastUID, true)
}

func (f IMAPFetcher) fetchSinceFolder(folder string, uidValidity, lastUID uint32, checkpointKnown bool) ([]Message, uint32, uint32, error) {
	c, err := f.connect()
	if err != nil {
		return nil, 0, lastUID, err
	}
	defer c.Logout()

	if err := c.Login(f.Username, f.Password); err != nil {
		return nil, 0, lastUID, err
	}
	mbox, err := c.Select(folder, true)
	if err != nil {
		return nil, 0, lastUID, err
	}
	if mbox.UidValidity == 0 {
		return nil, 0, lastUID, fmt.Errorf("uidvalidity missing")
	}
	initial := lastUID == 0
	if checkpointKnown {
		initial = uidValidity == 0 || uidValidity != mbox.UidValidity
	}
	if initial {
		lastUID = 0
		if f.InitialSyncMaxMessages != nil {
			limit := *f.InitialSyncMaxMessages
			if limit < -1 {
				return nil, mbox.UidValidity, 0, fmt.Errorf("initial sync limit must be -1 or greater")
			}
			if limit == 0 && mbox.UidNext > 0 {
				lastUID = mbox.UidNext - 1
			} else if limit > 0 && uint64(mbox.Messages) > uint64(limit) {
				// Sequence numbers count messages, unlike UIDs which may have gaps.
				seqset := new(imap.SeqSet)
				seqset.AddNum(mbox.Messages - uint32(limit) + 1)
				messages := make(chan *imap.Message, 1)
				done := make(chan error, 1)
				go func() { done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchUid}, messages) }()
				var firstUID uint32
				for m := range messages {
					if m != nil {
						firstUID = m.Uid
					}
				}
				if err := <-done; err != nil {
					return nil, mbox.UidValidity, 0, err
				}
				if firstUID == 0 {
					return nil, mbox.UidValidity, 0, fmt.Errorf("initial history boundary disappeared; retry sync")
				}
				lastUID = firstUID - 1
			}
		}
	}
	from, to, ok := fetchUIDRange(lastUID, mbox.UidNext, f.MaxFetch)
	if !ok {
		return nil, mbox.UidValidity, lastUID, nil
	}
	seqset := new(imap.SeqSet)
	seqset.AddRange(from, to)
	section := bodySectionForFetch()
	items := []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, section.FetchItem()}
	messages := make(chan *imap.Message, 50)
	errCh := make(chan error, 1)
	go func() { errCh <- c.UidFetch(seqset, items, messages) }()

	out := []Message{}
	// Even an entirely expunged UID range must advance, or the worker stalls.
	newLast := to
	var bodyErr error
	for m := range messages {
		if m == nil {
			continue
		}
		if m.Uid > newLast {
			newLast = m.Uid
		}
		r := m.GetBody(section)
		if r == nil {
			bodyErr = fmt.Errorf("missing message body for UID %d", m.Uid)
			continue
		}
		raw, err := io.ReadAll(r)
		if err != nil {
			bodyErr = fmt.Errorf("read message UID %d: %w", m.Uid, err)
			continue
		}
		out = append(out, Message{UID: m.Uid, InternalDate: m.InternalDate, Raw: raw})
	}
	if err := <-errCh; err != nil {
		return nil, mbox.UidValidity, lastUID, err
	}
	if bodyErr != nil {
		return nil, mbox.UidValidity, lastUID, bodyErr
	}
	return out, mbox.UidValidity, newLast, nil
}

func (f IMAPFetcher) WaitForChange(ctx context.Context, timeout time.Duration) (bool, error) {
	return f.WaitForChangeFolder(ctx, f.folder(), timeout)
}

func (f IMAPFetcher) WaitForChangeFolder(ctx context.Context, folder string, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	c, err := f.connect()
	if err != nil {
		return false, err
	}
	defer c.Logout()

	if err := c.Login(f.Username, f.Password); err != nil {
		return false, err
	}
	if _, err := c.Select(folder, false); err != nil {
		return false, err
	}

	updates := make(chan client.Update, 8)
	c.Updates = updates

	stop := make(chan struct{})
	var stopOnce sync.Once
	stopIdle := func() { stopOnce.Do(func() { close(stop) }) }

	done := make(chan error, 1)
	go func() {
		done <- c.Idle(stop, &client.IdleOptions{PollInterval: timeout})
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			stopIdle()
			<-done
			return false, ctx.Err()
		case <-timer.C:
			stopIdle()
			if err := <-done; err != nil {
				return false, err
			}
			return false, nil
		case <-updates:
			stopIdle()
			if err := <-done; err != nil {
				return false, err
			}
			return true, nil
		case err := <-done:
			stopIdle()
			return false, err
		}
	}
}

func (f IMAPFetcher) ListFolders() ([]string, error) {
	c, err := f.connect()
	if err != nil {
		return nil, err
	}
	defer c.Logout()

	if err := c.Login(f.Username, f.Password); err != nil {
		return nil, err
	}

	mailboxes := make(chan *imap.MailboxInfo, 100)
	errCh := make(chan error, 1)
	go func() { errCh <- c.List("", "*", mailboxes) }()

	folders := []string{}
	seen := map[string]struct{}{}
	for mb := range mailboxes {
		if mb == nil {
			continue
		}
		skip := false
		for _, attr := range mb.Attributes {
			if strings.EqualFold(attr, "\\Noselect") {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(mb.Name))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		folders = append(folders, mb.Name)
	}
	if err := <-errCh; err != nil {
		return nil, err
	}
	if len(folders) == 0 {
		return []string{f.folder()}, nil
	}
	return folders, nil
}

func (f IMAPFetcher) folder() string {
	if f.Folder == "" {
		return "INBOX"
	}
	return f.Folder
}

func (f IMAPFetcher) connect() (*client.Client, error) {
	addr := net.JoinHostPort(f.Host, strconv.Itoa(f.Port))
	if f.TLS {
		return client.DialTLS(addr, &tls.Config{ServerName: f.Host})
	}
	return client.Dial(addr)
}

func (f IMAPFetcher) dialContext(ctx context.Context) (net.Conn, error) {
	addr := net.JoinHostPort(f.Host, strconv.Itoa(f.Port))
	dialer := &net.Dialer{}
	if f.TLS {
		return (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: f.Host}}).DialContext(ctx, "tcp", addr)
	}
	return dialer.DialContext(ctx, "tcp", addr)
}

func bodySectionForFetch() *imap.BodySectionName {
	return &imap.BodySectionName{Peek: true}
}

func fetchUIDRange(lastUID, uidNext uint32, maxFetch int) (from uint32, to uint32, ok bool) {
	if uidNext == 0 || lastUID >= uidNext-1 {
		return 0, 0, false
	}
	from = lastUID + 1
	to = uidNext - 1
	if maxFetch > 0 && uint64(maxFetch) < uint64(to)-uint64(from)+1 {
		to = from + uint32(maxFetch) - 1
	}
	return from, to, true
}
