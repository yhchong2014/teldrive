package telegramstore

import (
	"sort"
	"sync"
	"time"
)

// AccountStreamStats reports what one Telegram account delivered while serving
// a download. Media requests rotate across every leased account, so per-account
// throughput is what tells an operator which bot is fast and which one stalls.
type AccountStreamStats struct {
	AccountID int64
	Bytes     int64
	Chunks    int
	Elapsed   time.Duration
}

// MBps is the average payload throughput of the account in mebibytes per
// second. It counts only the time spent inside the Telegram request, so a slow
// account is not hidden by time the reader spent waiting on the network.
func (s AccountStreamStats) MBps() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.Bytes) / s.Elapsed.Seconds() / (1 << 20)
}

// StreamStatsProvider is implemented by download sessions that track per-account
// transfer counters. The HTTP layer logs them when a stream ends.
type StreamStatsProvider interface {
	StreamStats() []AccountStreamStats
}

// streamStats accumulates per-account counters for one download session. It is
// safe for concurrent use because chunk requests run in parallel.
type streamStats struct {
	mu       sync.Mutex
	accounts map[int64]*AccountStreamStats
}

func newStreamStats() *streamStats {
	return &streamStats{accounts: make(map[int64]*AccountStreamStats)}
}

func (s *streamStats) record(accountID, bytes int64, elapsed time.Duration) {
	if s == nil || accountID <= 0 || bytes <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts == nil {
		s.accounts = make(map[int64]*AccountStreamStats)
	}
	entry := s.accounts[accountID]
	if entry == nil {
		entry = &AccountStreamStats{AccountID: accountID}
		s.accounts[accountID] = entry
	}
	entry.Bytes += bytes
	entry.Chunks++
	entry.Elapsed += elapsed
}

// snapshot returns a copy ordered by account ID so log lines are stable.
func (s *streamStats) snapshot() []AccountStreamStats {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.accounts) == 0 {
		return nil
	}
	out := make([]AccountStreamStats, 0, len(s.accounts))
	for _, entry := range s.accounts {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out
}
