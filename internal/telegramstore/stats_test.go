package telegramstore

import (
	"testing"
	"time"
)

func TestStreamStatsAggregatesPerAccount(t *testing.T) {
	t.Parallel()

	stats := newStreamStats()
	if got := stats.snapshot(); got != nil {
		t.Fatalf("empty snapshot = %#v, want nil", got)
	}

	stats.record(202, 512*1024, time.Second)
	stats.record(101, 256*1024, 500*time.Millisecond)
	stats.record(202, 512*1024, time.Second)
	// Ignored: no account, no bytes, or no recorder.
	stats.record(0, 1024, time.Second)
	stats.record(303, 0, time.Second)
	var nilStats *streamStats
	nilStats.record(404, 1024, time.Second)

	got := stats.snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(got))
	}
	if got[0].AccountID != 101 || got[1].AccountID != 202 {
		t.Fatalf("snapshot order = %d,%d, want 101,202", got[0].AccountID, got[1].AccountID)
	}
	if got[1].Bytes != 1024*1024 || got[1].Chunks != 2 {
		t.Fatalf("account 202 = %d bytes in %d chunks, want %d bytes in 2 chunks", got[1].Bytes, got[1].Chunks, 1024*1024)
	}
	if got[1].Elapsed != 2*time.Second {
		t.Fatalf("account 202 elapsed = %v, want 2s", got[1].Elapsed)
	}
	if mbps := got[1].MBps(); mbps < 0.49 || mbps > 0.51 {
		t.Fatalf("account 202 MBps = %v, want 0.5", mbps)
	}
}

func TestAccountStreamStatsMBpsHandlesZeroElapsed(t *testing.T) {
	t.Parallel()

	if got := (AccountStreamStats{Bytes: 1024}).MBps(); got != 0 {
		t.Fatalf("MBps() = %v, want 0 for zero elapsed", got)
	}
}
