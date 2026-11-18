package imapingest

import "testing"

func TestBodySectionForFetchUsesPeek(t *testing.T) {
	section := bodySectionForFetch()
	if !section.Peek {
		t.Fatalf("expected IMAP fetch to use BODY.PEEK to avoid setting \\\\Seen")
	}
}

func TestFetchRangeNoCap(t *testing.T) {
	from, to, ok := fetchUIDRange(100, 105, 0)
	if !ok {
		t.Fatalf("expected range")
	}
	if from != 101 || to != 104 {
		t.Fatalf("unexpected range from=%d to=%d", from, to)
	}
}

func TestFetchRangeWithCap(t *testing.T) {
	from, to, ok := fetchUIDRange(100, 500, 50)
	if !ok {
		t.Fatalf("expected range")
	}
	if from != 101 || to != 150 {
		t.Fatalf("unexpected capped range from=%d to=%d", from, to)
	}
}

func TestFetchRangeEmpty(t *testing.T) {
	_, _, ok := fetchUIDRange(100, 101, 50)
	if ok {
		t.Fatalf("expected empty range")
	}
}

func TestFetchRangeDoesNotWrapUIDs(t *testing.T) {
	if _, _, ok := fetchUIDRange(^uint32(0), 5, 200); ok {
		t.Fatal("exhausted checkpoint must not wrap to first message")
	}
	from, to, ok := fetchUIDRange(^uint32(0)-10, ^uint32(0), 200)
	if !ok || from != ^uint32(0)-9 || to != ^uint32(0)-1 {
		t.Fatalf("range overflow: %d:%d valid=%v", from, to, ok)
	}
}
