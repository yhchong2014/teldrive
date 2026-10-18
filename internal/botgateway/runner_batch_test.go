package botgateway

import (
	"testing"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

func TestMakeDownloadLeasesPinsPreferredAndSplitsConnections(t *testing.T) {
	runner := &UploadAwareRunner{}
	bots := []*sqlcgen.Bot{{BotID: 11}, {BotID: 22}, {BotID: 33}}
	leases := runner.makeDownloadLeases(bots, 99, telegramstore.OperationDownload, 32, 16, 22, 0)
	if len(leases) != 3 {
		t.Fatalf("leases = %d, want 3", len(leases))
	}
	if got := leases[0].AccountID(); got != 22 {
		t.Fatalf("preferred account = %d, want 22", got)
	}
	seen := make(map[int64]bool, len(leases))
	for _, lease := range leases {
		if seen[lease.AccountID()] {
			t.Fatalf("duplicate account lease %d", lease.AccountID())
		}
		seen[lease.AccountID()] = true
		botLease, ok := lease.(*botLease)
		if !ok {
			t.Fatalf("lease for %d has type %T, want *botLease", lease.AccountID(), lease)
		}
		if botLease.connections != 6 {
			t.Errorf("connections for bot %d = %d, want ceil(16/3)=6", botLease.bot.BotID, botLease.connections)
		}
	}
	for _, lease := range leases {
		lease.Release()
	}
	if got := runner.load.connections; len(got) != 0 {
		t.Fatalf("load reservations after release = %v, want none", got)
	}
}

func TestMakeDownloadLeasesIncludesPreferredUserFallback(t *testing.T) {
	runner := &UploadAwareRunner{}
	bots := []*sqlcgen.Bot{{BotID: 11}, {BotID: 22}, {BotID: 33}}
	leases := runner.makeDownloadLeases(bots, 99, telegramstore.OperationDownload, 4, 16, 99, 0)
	if len(leases) != 4 {
		t.Fatalf("leases = %d, want preferred user plus 3 bots", len(leases))
	}
	if got := leases[0].AccountID(); got != 99 {
		t.Fatalf("first account = %d, want preferred user 99", got)
	}
	seen := make(map[int64]bool, len(leases))
	for _, lease := range leases {
		if seen[lease.AccountID()] {
			t.Fatalf("duplicate account lease %d", lease.AccountID())
		}
		seen[lease.AccountID()] = true
		if lease, ok := lease.(*botLease); ok && lease.connections != 4 {
			t.Errorf("connections for bot %d = %d, want 4", lease.bot.BotID, lease.connections)
		}
	}
	for _, lease := range leases {
		lease.Release()
	}
}

func TestMakeDownloadLeasesFallsBackToUserWhenNoBots(t *testing.T) {
	runner := &UploadAwareRunner{}
	leases := runner.makeDownloadLeases(nil, 99, telegramstore.OperationDownload, 32, 16, 123, 0)
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1 fallback lease", len(leases))
	}
	if got := leases[0].AccountID(); got != 99 {
		t.Fatalf("fallback account = %d, want user 99", got)
	}
}
