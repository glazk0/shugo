// Package history keeps a short, in-memory record of each member's recent
// messages so the moderator can judge a message in context (repeated spam,
// channel hopping, escalating harassment) without querying Discord.
package history

import (
	"context"
	"sync"
	"time"
)

// Entry is a single remembered message.
type Entry struct {
	MessageID string
	ChannelID string
	Content   string
	At        time.Time
}

type key struct {
	guildID string
	userID  string
}

// Store is a bounded, TTL-based message history keyed by guild and user. It
// is safe for concurrent use. Memory is bounded per user by size, and idle
// users are dropped by Prune once all their entries have expired.
type Store struct {
	mu      sync.Mutex
	size    int
	ttl     time.Duration
	entries map[key][]Entry
}

// New returns a Store that keeps at most size entries per user, each for at
// most ttl.
//
// Parameters:
//   - size (int): maximum entries kept per guild member; values < 1 become 1.
//   - ttl (time.Duration): how long an entry stays visible after Entry.At.
func New(size int, ttl time.Duration) *Store {
	return &Store{
		size:    max(size, 1),
		ttl:     ttl,
		entries: make(map[key][]Entry),
	}
}

// Add records e for the given member, evicting the oldest entry when the
// member's history is full.
//
// Parameters:
//   - guildID (string): guild the message was sent in.
//   - userID (string): author of the message.
//   - e (Entry): the message to remember.
func (s *Store) Add(guildID, userID string, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.append(key{guildID, userID}, e)
}

// Record stores e for the given member and returns the member's other
// unexpired entries as they were before e arrived, oldest first. Both happen
// under one lock, so concurrent messages from the same member always see each
// other. An entry with the same MessageID, as left by an edited message, is
// replaced in place rather than duplicated, and is not returned.
//
// Parameters:
//   - guildID (string): guild the message was sent in.
//   - userID (string): author of the message.
//   - e (Entry): the message to remember.
//   - now (time.Time): reference time used to apply the TTL.
func (s *Store) Record(guildID, userID string, e Entry, now time.Time) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key{guildID, userID}
	list := s.entries[k]
	prior := make([]Entry, 0, len(list))
	replaced := false
	for i, old := range list {
		if e.MessageID != "" && old.MessageID == e.MessageID {
			list[i] = e
			replaced = true
			continue
		}
		if s.alive(old, now) {
			prior = append(prior, old)
		}
	}
	if !replaced {
		s.append(k, e)
	}
	return prior
}

// Recent returns a copy of the member's unexpired entries, oldest first.
//
// Parameters:
//   - guildID (string): guild to look in.
//   - userID (string): member to look up.
//   - now (time.Time): reference time used to apply the TTL.
func (s *Store) Recent(guildID, userID string, now time.Time) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	list := s.entries[key{guildID, userID}]
	out := make([]Entry, 0, len(list))
	for _, e := range list {
		if s.alive(e, now) {
			out = append(out, e)
		}
	}
	return out
}

// Prune drops expired entries and forgets members with nothing left. It
// returns the number of members removed.
//
// Parameters:
//   - now (time.Time): reference time used to apply the TTL.
func (s *Store) Prune(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for k, list := range s.entries {
		// Entries are appended in arrival order, so expired ones form a prefix.
		i := 0
		for i < len(list) && !s.alive(list[i], now) {
			i++
		}
		switch {
		case i == len(list):
			delete(s.entries, k)
			removed++
		case i > 0:
			s.entries[k] = append([]Entry(nil), list[i:]...)
		}
	}
	return removed
}

// Len returns the number of members currently tracked.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Run calls Prune every interval until ctx is cancelled.
//
// Parameters:
//   - ctx (context.Context): stops the loop when done.
//   - interval (time.Duration): time between prunes; must be positive.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.Prune(now)
		}
	}
}

// append adds e to the member's list, evicting the oldest entry when the
// list is full. The caller must hold s.mu.
//
// Parameters:
//   - k (key): member to append to.
//   - e (Entry): the message to remember.
func (s *Store) append(k key, e Entry) {
	list := s.entries[k]
	list = append(list, e)
	if len(list) > s.size {
		// Copy into a fresh slice so the evicted prefix can be collected.
		list = append([]Entry(nil), list[len(list)-s.size:]...)
	}
	s.entries[k] = list
}

// alive reports whether e is still within the TTL at now.
//
// Parameters:
//   - e (Entry): entry to check.
//   - now (time.Time): reference time.
func (s *Store) alive(e Entry, now time.Time) bool {
	return now.Sub(e.At) < s.ttl
}
