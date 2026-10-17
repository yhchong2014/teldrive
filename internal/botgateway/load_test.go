package botgateway

import (
	"testing"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

func testBots(ids ...int64) []*sqlcgen.Bot {
	bots := make([]*sqlcgen.Bot, len(ids))
	for i, id := range ids {
		bots[i] = &sqlcgen.Bot{BotID: id}
	}
	return bots
}

func preferBots(ids ...int64) func(int64) bool {
	set := make(map[int64]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return func(id int64) bool { return set[id] }
}

func TestBotLoadRotatesEquallyLoadedBotsWithoutPreference(t *testing.T) {
	var load botLoad
	bots := testBots(1, 2, 3)
	for rotation, want := range []int64{1, 2, 3} {
		bot := load.acquire(bots, uint64(rotation), 4, nil)
		if bot.BotID != want {
			t.Fatalf("rotation %d picked bot %d, want %d", rotation, bot.BotID, want)
		}
		load.release(bot.BotID, 4)
	}
}

func TestBotLoadPrefersWarmBotAmongIdleBots(t *testing.T) {
	var load botLoad
	bots := testBots(1, 2, 3)
	// Whatever the rotation, the idle warm bot wins over idle cold bots.
	for rotation := range uint64(3) {
		bot := load.acquire(bots, rotation, 4, preferBots(3))
		if bot.BotID != 3 {
			t.Fatalf("rotation %d picked bot %d, want warm bot 3", rotation, bot.BotID)
		}
		load.release(bot.BotID, 4)
	}
}

func TestBotLoadKeepsOverlappingRequestOnWarmBot(t *testing.T) {
	var load botLoad
	bots := testBots(1, 2)
	// A player streams from warm bot 2 and opens the range after a seek before
	// closing the old one: the new request must stay on bot 2.
	first := load.acquire(bots, 0, 4, preferBots(2))
	second := load.acquire(bots, 0, 4, preferBots(2))
	if first.BotID != 2 || second.BotID != 2 {
		t.Fatalf("picked bots %d and %d, want warm bot 2 twice", first.BotID, second.BotID)
	}
	// A third concurrent stream spreads to the idle bot.
	if third := load.acquire(bots, 0, 4, preferBots(2)); third.BotID != 1 {
		t.Fatalf("third stream picked bot %d, want idle bot 1", third.BotID)
	}
}

func TestBotLoadStillPrefersLessLoadedColdBot(t *testing.T) {
	var load botLoad
	bots := testBots(1, 2)
	// An upload keeps eight connections busy on the warm bot.
	load.connections = map[int64]int{1: 8}
	if bot := load.acquire(bots, 0, 4, preferBots(1)); bot.BotID != 2 {
		t.Fatalf("picked bot %d, want the idle cold bot 2", bot.BotID)
	}
}
