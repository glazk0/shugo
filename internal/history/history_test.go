package history_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/glazk0/shugo/internal/history"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// entry builds an Entry sent offset after t0.
//
// Parameters:
//   - id (string): message ID, also used as content.
//   - offset (time.Duration): time after t0.
func entry(id string, offset time.Duration) history.Entry {
	return history.Entry{MessageID: id, ChannelID: "c", Content: id, At: t0.Add(offset)}
}

// ids extracts message IDs in order.
//
// Parameters:
//   - entries ([]history.Entry): entries to summarise.
func ids(entries []history.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.MessageID
	}
	return out
}

func TestRecentKeepsNewestEntriesUpToSize(t *testing.T) {
	t.Parallel()

	s := history.New(3, time.Hour)
	for i := range 5 {
		s.Add("g", "u", entry(fmt.Sprint(i), time.Duration(i)*time.Second))
	}

	got := ids(s.Recent("g", "u", t0.Add(time.Minute)))
	want := []string{"2", "3", "4"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Recent() = %v, want %v", got, want)
	}
}

func TestRecentIsScopedToGuildAndUser(t *testing.T) {
	t.Parallel()

	s := history.New(5, time.Hour)
	s.Add("g1", "u1", entry("a", 0))
	s.Add("g1", "u2", entry("b", 0))
	s.Add("g2", "u1", entry("c", 0))

	if got := ids(s.Recent("g1", "u1", t0)); len(got) != 1 || got[0] != "a" {
		t.Errorf("Recent(g1, u1) = %v, want [a]", got)
	}
	if got := s.Recent("g3", "u1", t0); len(got) != 0 {
		t.Errorf("Recent(unknown guild) = %v, want empty", got)
	}
}

func TestRecentHidesExpiredEntries(t *testing.T) {
	t.Parallel()

	s := history.New(5, time.Minute)
	s.Add("g", "u", entry("old", 0))
	s.Add("g", "u", entry("new", 50*time.Second))

	got := ids(s.Recent("g", "u", t0.Add(70*time.Second)))
	if len(got) != 1 || got[0] != "new" {
		t.Errorf("Recent() = %v, want [new]", got)
	}
}

func TestRecentReturnsCopy(t *testing.T) {
	t.Parallel()

	s := history.New(5, time.Hour)
	s.Add("g", "u", entry("a", 0))

	got := s.Recent("g", "u", t0)
	got[0].Content = "mutated"

	if s.Recent("g", "u", t0)[0].Content != "a" {
		t.Error("mutating Recent() result changed the store")
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()

	s := history.New(5, time.Minute)
	s.Add("g", "stale", entry("a", 0))
	s.Add("g", "mixed", entry("b", 0))
	s.Add("g", "mixed", entry("c", 45*time.Second))

	if removed := s.Prune(t0.Add(90 * time.Second)); removed != 1 {
		t.Errorf("Prune() removed %d members, want 1", removed)
	}
	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
	if got := ids(s.Recent("g", "mixed", t0.Add(90*time.Second))); len(got) != 1 || got[0] != "c" {
		t.Errorf("Recent(mixed) = %v, want [c]", got)
	}
}

func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	s := history.New(10, time.Minute)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 200 {
				user := fmt.Sprint(w % 3)
				s.Add("g", user, entry(fmt.Sprint(i), time.Duration(i)*time.Millisecond))
				_ = s.Recent("g", user, t0)
				if i%50 == 0 {
					s.Prune(t0)
				}
			}
		})
	}
	wg.Wait()

	if s.Len() != 3 {
		t.Errorf("Len() = %d, want 3", s.Len())
	}
}

func TestRecordReturnsPriorEntries(t *testing.T) {
	t.Parallel()

	s := history.New(5, time.Minute)
	if got := s.Record("g", "u", entry("old", 0), t0); len(got) != 0 {
		t.Errorf("Record(first) = %v, want empty", ids(got))
	}
	s.Record("g", "u", entry("a", 50*time.Second), t0.Add(50*time.Second))

	got := ids(s.Record("g", "u", entry("b", 70*time.Second), t0.Add(70*time.Second)))
	if fmt.Sprint(got) != "[a]" {
		t.Errorf("Record() = %v, want [a] (expired entry hidden, new entry excluded)", got)
	}
	if got := ids(s.Recent("g", "u", t0.Add(70*time.Second))); fmt.Sprint(got) != "[a b]" {
		t.Errorf("Recent() = %v, want [a b]", got)
	}
}

func TestRecordReplacesEditedMessage(t *testing.T) {
	t.Parallel()

	s := history.New(5, time.Hour)
	s.Record("g", "u", entry("1", 0), t0)
	s.Record("g", "u", entry("2", time.Second), t0)

	edited := entry("1", 0)
	edited.Content = "edited"
	got := s.Record("g", "u", edited, t0.Add(time.Minute))
	if fmt.Sprint(ids(got)) != "[2]" {
		t.Errorf("Record(edit) = %v, want [2] without the edited message itself", ids(got))
	}

	all := s.Recent("g", "u", t0.Add(time.Minute))
	if len(all) != 2 || all[0].MessageID != "1" || all[0].Content != "edited" {
		t.Errorf("Recent() = %+v, want the edit to replace message 1 in place", all)
	}
}

func TestRecordSeesConcurrentMessages(t *testing.T) {
	t.Parallel()

	const n = 20
	s := history.New(n, time.Hour)
	seen := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			seen[i] = len(s.Record("g", "u", entry(fmt.Sprint(i), 0), t0))
		})
	}
	wg.Wait()

	// Each call must observe every call that completed before it, so the
	// prior counts are exactly 0, 1, …, n-1 in some order.
	counts := make(map[int]bool, n)
	for _, c := range seen {
		counts[c] = true
	}
	if len(counts) != n {
		t.Errorf("prior counts = %v, want each of 0..%d exactly once", seen, n-1)
	}
}
