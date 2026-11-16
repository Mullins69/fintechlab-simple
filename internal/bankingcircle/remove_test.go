package bankingcircle

import (
	"errors"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// parkSix gives sub six notifications the dispatcher holds: two in the
// batch filling up (quiet's flush waits an hour), then a pause, and four
// parked behind it.
func parkSix(t *testing.T, b *testBank, sub *Subscription) {
	t.Helper()
	for i := range 6 {
		if i == 2 {
			b.disp.Pause(sub.ID)
		}
		b.disp.Enqueue(sub, Notification{EventID: string(rune('a' + i))})
	}
	b.disp.Wait()
	b.disp.mu.Lock()
	batching := len(b.disp.queues[sub.ID].items)
	b.disp.mu.Unlock()
	if parked := b.disp.Queued(sub.ID); parked != 4 || batching != 2 {
		t.Fatalf("%d parked and %d batching, want 4 and 2 before the test starts", parked, batching)
	}
}

func TestTheLabRemovesASubscriptionWithWhatWasWaitingForIt(t *testing.T) {
	b := newTestBank(t, time.Hour, quiet())
	platform := subscribeEverything(t, b.subs, "https://example.test/platform", 5)
	parkSix(t, b, platform)

	gone, dropped, err := b.bank.RemoveSubscription(tenant.Default, platform.ID, nil)
	if err != nil {
		t.Fatalf("the lab's delete: %v", err)
	}
	if gone.ID != platform.ID || dropped != 6 {
		t.Errorf("removed %s with %d notification(s), want %s with all 6", gone.ID, dropped, platform.ID)
	}
	if _, err := b.subs.Get(platform.ID); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("the subscription is still there: %v", err)
	}
	if b.disp.Paused(platform.ID) || b.disp.Queued(platform.ID) != 0 || b.disp.Pending(platform.ID) != 0 {
		t.Errorf("the dispatcher still holds the deleted subscription: paused=%v queued=%d pending=%d",
			b.disp.Paused(platform.ID), b.disp.Queued(platform.ID), b.disp.Pending(platform.ID))
	}
	b.disp.mu.Lock()
	_, batching := b.disp.queues[platform.ID]
	b.disp.mu.Unlock()
	if batching {
		t.Error("the batch filling up for the deleted subscription is still there, with its flush timer")
	}
	if _, err := b.subs.Get(b.seed.ID); err != nil {
		t.Errorf("deleting one subscription took another with it: %v", err)
	}
}

func TestTheVendorsDeleteStillWantsTheRowVersion(t *testing.T) {
	b := newTestBank(t, time.Hour, quiet())
	stale := "not-the-row-version"
	if _, _, err := b.bank.RemoveSubscription(tenant.Default, b.seed.ID, &stale); !errors.Is(err, ErrRowVersionMismatch) {
		t.Fatalf("a stale If-Match: %v, want ErrRowVersionMismatch", err)
	}
	current := b.seed.RowVersion
	if _, _, err := b.bank.RemoveSubscription(tenant.Default, b.seed.ID, &current); err != nil {
		t.Fatalf("the current rowVersion: %v", err)
	}
}

func TestRemovingASubscriptionStaysInItsEnvironment(t *testing.T) {
	b := newTestBank(t, time.Hour, quiet())
	sbx, err := b.subs.Create(CreateParams{Env: "sbx", Endpoint: "https://example.test/sbx",
		EncryptionKey: "0123456789abcdef0123456789abcdef", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.bank.RemoveSubscription(tenant.Default, sbx.ID, nil); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("the default environment deleted sbx's subscription: %v", err)
	}
	if _, _, err := b.bank.RemoveSubscription("sbx", b.seed.ID, nil); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("sbx deleted the default environment's subscription: %v", err)
	}
	if _, _, err := b.bank.RemoveSubscription("sbx", sbx.ID, nil); err != nil {
		t.Fatalf("sbx deleting its own: %v", err)
	}
}

func TestResettingSubscriptionsPutsBackTheSeedAndNothingElse(t *testing.T) {
	b := newTestBank(t, time.Hour, quiet())
	// Money the reset must leave where it is (before the platform
	// subscribes, so none of its notifications are the platform's).
	if _, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "100.00", "lump sum"); err != nil {
		t.Fatal(err)
	}
	// What runs leave behind: a platform's subscription with notifications
	// parked behind a pause, and the seeded one written to.
	platform := subscribeEverything(t, b.subs, "https://example.test/platform", 5)
	parkSix(t, b, platform)
	if _, err := b.subs.OverrideActive(b.seed.ID, false); err != nil {
		t.Fatal(err)
	}
	sbx, err := b.subs.Create(CreateParams{Env: "sbx", Endpoint: "https://example.test/sbx",
		EncryptionKey: "0123456789abcdef0123456789abcdef", Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}

	cleared := b.bank.ResetSubscriptions(tenant.Default)
	if cleared["subscriptions"] != 2 || cleared["notifications"] != 6 || cleared["pauses"] != 1 {
		t.Errorf("cleared %v, want 2 subscriptions (the platform's gone, the seed's put back), 6 notifications, 1 pause", cleared)
	}
	subs, _ := b.subs.ListIn(tenant.Default, 1, 100)
	if len(subs) != 1 || subs[0].ID != b.seed.ID || !subs[0].IsActive {
		t.Fatalf("the default environment holds %v, want the seed alone, active again", subs)
	}
	if _, err := b.subs.GetIn("sbx", sbx.ID); err != nil {
		t.Errorf("a reset of the default environment took sbx's subscription: %v", err)
	}
	if b.disp.Paused(platform.ID) || b.disp.Queued(platform.ID) != 0 {
		t.Error("the dispatcher still holds the platform's deleted subscription")
	}
	if got := balance(t, b.ledger, SGAAccountEUR); got != "100.00" {
		t.Errorf("the safeguarding balance is %s after a subscriptions reset, want 100.00 untouched", got)
	}

	if got := b.bank.ResetSubscriptions("sbx"); got["subscriptions"] != 1 {
		t.Errorf("sbx's reset cleared %v, want its one subscription: a customer has no seed", got)
	}
	if _, err := b.subs.GetIn("sbx", sbx.ID); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("sbx's subscription survived sbx's reset: %v", err)
	}
}
