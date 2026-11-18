package imapingest

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

// A minimal wire fixture keeps these tests independent of extra backend packages.
func historyFetcher(t *testing.T, uids []uint32, limit int) IMAPFetcher {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				fmt.Fprint(conn, "* OK test IMAP ready\r\n")
				scanner := bufio.NewScanner(conn)
				for scanner.Scan() {
					fields := strings.Fields(scanner.Text())
					if len(fields) < 2 {
						return
					}
					tag, command := fields[0], strings.ToUpper(fields[1])
					switch command {
					case "CAPABILITY":
						fmt.Fprint(conn, "* CAPABILITY IMAP4rev1\r\n")
					case "LOGIN":
					case "SELECT", "EXAMINE":
						next := uint32(1)
						if len(uids) > 0 {
							next = uids[len(uids)-1] + 1
						}
						fmt.Fprintf(conn, "* FLAGS (\\Seen)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n* OK [UIDNEXT %d] next\r\n", len(uids), next)
					case "FETCH", "UID":
						rangeIndex := 2
						if command == "UID" {
							rangeIndex = 3
						}
						ends := strings.Split(fields[rangeIndex], ":")
						low, _ := strconv.ParseUint(ends[0], 10, 32)
						high := low
						if len(ends) > 1 {
							high, _ = strconv.ParseUint(ends[1], 10, 32)
						}
						for index, uid := range uids {
							id := uint64(index + 1)
							if command == "UID" {
								id = uint64(uid)
							}
							if id < low || id > high {
								continue
							}
							if command == "FETCH" {
								fmt.Fprintf(conn, "* %d FETCH (UID %d)\r\n", index+1, uid)
							} else {
								raw := fmt.Sprintf("From: sender@example.org\r\nTo: user+agent@example.org\r\nSubject: %d\r\n\r\nbody", uid)
								fmt.Fprintf(conn, "* %d FETCH (UID %d INTERNALDATE \"26-Sep-2026 10:00:00 +0000\" BODY[] {%d}\r\n%s)\r\n", index+1, uid, len(raw), raw)
							}
						}
					case "LOGOUT":
						fmt.Fprintf(conn, "* BYE bye\r\n%s OK done\r\n", tag)
						return
					default:
						fmt.Fprintf(conn, "%s BAD unsupported\r\n", tag)
						continue
					}
					fmt.Fprintf(conn, "%s OK done\r\n", tag)
				}
			}()
		}
	}()
	return IMAPFetcher{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, Username: "username", Password: "password", MaxFetch: 200, InitialSyncMaxMessages: &limit}
}

func TestInitialHistoryBoundPersistsAcrossCycles(t *testing.T) {
	f := historyFetcher(t, []uint32{1, 100, 500, 501, 502}, 2)
	f.MaxFetch = 1
	msgs, validity, last, err := f.FetchSinceCheckpoint("INBOX", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].UID != 501 || last != 501 {
		t.Fatalf("first: messages=%v last=%d", msgs, last)
	}
	msgs, _, last, err = f.FetchSinceCheckpoint("INBOX", validity, last)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].UID != 502 || last != 502 {
		t.Fatalf("second: messages=%v last=%d", msgs, last)
	}
	msgs, _, _, err = f.FetchSinceCheckpoint("INBOX", validity, last)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("third: messages=%v err=%v", msgs, err)
	}
}

func TestNewOnlyHistoryAndEmptyMailbox(t *testing.T) {
	f := historyFetcher(t, []uint32{1, 10}, 0)
	msgs, _, last, err := f.FetchSinceCheckpoint("INBOX", 0, 0)
	if err != nil || len(msgs) != 0 || last != 10 {
		t.Fatalf("new only: messages=%v last=%d err=%v", msgs, last, err)
	}
	// A stored UIDVALIDITY means even lastUID=0 is an initialized mailbox.
	f = historyFetcher(t, []uint32{1}, 0)
	msgs, _, last, err = f.FetchSinceCheckpoint("INBOX", 1, 0)
	if err != nil || len(msgs) != 1 || last != 1 {
		t.Fatalf("first arrival: messages=%v last=%d err=%v", msgs, last, err)
	}
}

func TestHistoryResetAndFullImport(t *testing.T) {
	f := historyFetcher(t, []uint32{1, 10, 20}, 1)
	msgs, validity, last, err := f.FetchSinceCheckpoint("INBOX", 42, 999)
	if err != nil || validity != 1 || len(msgs) != 1 || msgs[0].UID != 20 || last != 20 {
		t.Fatalf("reset: messages=%v validity=%d last=%d err=%v", msgs, validity, last, err)
	}
	all := -1
	f.InitialSyncMaxMessages = &all
	msgs, _, _, err = f.FetchSinceCheckpoint("INBOX", 0, 0)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("all: messages=%v err=%v", msgs, err)
	}
}

func TestSparseUIDRangeAdvancesCheckpoint(t *testing.T) {
	f := historyFetcher(t, []uint32{500}, -1)
	f.MaxFetch = 200
	msgs, _, last, err := f.FetchSinceCheckpoint("INBOX", 1, 1)
	if err != nil || len(msgs) != 0 || last != 201 {
		t.Fatalf("sparse: messages=%v last=%d err=%v", msgs, last, err)
	}
}
