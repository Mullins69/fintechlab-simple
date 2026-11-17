package bankingcircle

import (
	"context"
	"errors"
	"sync"
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

func TestDeletingASubscriptionEndsItsRelease(t *testing.T) {
	b := newTestBank(t, time.Hour, quiet())
	// Twenty parked, released as four batches of five.
	platform := subscribeEverything(t, b.subs, "https://example.test/platform", 5)
	b.disp.Pause(platform.ID)
	for i := range 20 {
		b.disp.Enqueue(platform, Notification{EventID: string(rune('a' + i))})
	}

	// The release's first batch hangs on the wire until the delete is done.
	onTheWire, letGo := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	sent := 0
	b.disp.Send = func(context.Context, *Subscription, Envelope) error {
		mu.Lock()
		sent++
		first := sent == 1
		mu.Unlock()
		if first {
			close(onTheWire)
			<-letGo
		}
		return nil
	}
	if released := b.disp.Resume(platform); released != 20 {
		t.Fatalf("released %d, want the 20 parked", released)
	}
	select {
	case <-onTheWire:
	case <-time.After(2 * time.Second):
		t.Fatal("the release never sent its first batch")
	}

	_, dropped, err := b.bank.RemoveSubscription(tenant.Default, platform.ID, nil)
	close(letGo)
	if err != nil {
		t.Fatalf("the lab's delete: %v", err)
	}
	within(t, 2*time.Second, "the release", b.disp.Wait)
	mu.Lock()
	defer mu.Unlock()
	if sent != 1 {
		t.Errorf("%d batches reached the deleted subscription's endpoint, want only the one already on the wire", sent)
	}
	if dropped != 20 {
		t.Errorf("dropped %d, want 20: the batch on the wire and the three behind it", dropped)
	}
	b.disp.mu.Lock()
	inflight := len(b.disp.inflight)
	b.disp.mu.Unlock()
	if inflight != 0 {
		t.Errorf("%d batch(es) still counted on their way out to a deleted subscription", inflight)
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

func TestResettingSubscriptionsPutsTheSeedBackWithNothingHeld(t *testing.T) {
	b := newTestBank(t, time.Hour, quiet())
	// What a run leaves on the seed: one notification batching (quiet's
	// flush waits an hour), two parked behind a pause, and three retained
	// after the retry schedule gave up.
	b.disp.Enqueue(b.seed, Notification{EventID: "batching"})
	b.disp.Pause(b.seed.ID)
	b.disp.Enqueue(b.seed, Notification{EventID: "parked-1"})
	b.disp.Enqueue(b.seed, Notification{EventID: "parked-2"})
	b.disp.retain(b.seed, []Notification{{EventID: "r1"}, {EventID: "r2"}, {EventID: "r3"}})

	cleared := b.bank.ResetSubscriptions(tenant.Default)
	if cleared["notifications"] != 5 || cleared["pauses"] != 1 {
		t.Errorf("cleared %v, want the seed's 5 parked and retained notifications and its pause", cleared)
	}
	if b.disp.Paused(b.seed.ID) || b.disp.Queued(b.seed.ID) != 0 || b.disp.Pending(b.seed.ID) != 0 {
		t.Errorf("the seed came back holding paused=%v queued=%d pending=%d, want nothing",
			b.disp.Paused(b.seed.ID), b.disp.Queued(b.seed.ID), b.disp.Pending(b.seed.ID))
	}
	b.disp.mu.Lock()
	batching := len(b.disp.queues[b.seed.ID].items)
	b.disp.mu.Unlock()
	if batching != 1 {
		t.Errorf("%d notification(s) batching for the seed, want its 1 still on its way", batching)
	}
}
