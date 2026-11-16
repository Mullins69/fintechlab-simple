package bankingcircle

import (
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Removing a subscription is the vendor's DELETE, and the lab's two ways in
// on the control port: one subscription at a time without If-Match, and
// every subscription of an environment back to how start-up left them.
//
// Either way the dispatcher forgets what it held for a subscription that is
// gone: the batch filling up, the notifications parked behind a pause and
// the pause itself, and what it retained after giving up on the endpoint.
// Left behind, they would sit in the state file for good -- nothing can
// release a pause on a subscription nobody can name -- and a subscription
// put back later under the same id would inherit them. A batch already on
// its way out is not cut short: its retries find the subscription gone and
// stop (Dispatcher.deliverSync), as they always have.

// RemoveSubscription deletes subscription id, scope's, and what the
// dispatcher held for it. ifMatch is the vendor's concurrency check: nil
// is the lab's override, which takes any rowVersion. It answers the
// subscription as it was and how many notifications went with it;
// ErrSubscriptionNotFound for another environment's, as for an unknown one.
func (b *Bank) RemoveSubscription(scope, id string, ifMatch *string) (*Subscription, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disp.mu.Lock()
	b.subs.mu.Lock()
	sub, ok := b.subs.subs[id]
	if !ok || !tenant.InScope(tenant.Normalize(scope), sub.Env) {
		b.subs.mu.Unlock()
		b.disp.mu.Unlock()
		return nil, 0, ErrSubscriptionNotFound
	}
	if ifMatch != nil {
		if err := checkRowVersion(sub.RowVersion, *ifMatch); err != nil {
			b.subs.mu.Unlock()
			b.disp.mu.Unlock()
			return nil, 0, err
		}
	}
	b.subs.deleteLocked(id)
	dropped, _ := b.disp.forgetLocked(id)
	b.subs.mu.Unlock()
	b.disp.mu.Unlock()
	changed(b.subs.OnChange)
	changed(b.disp.OnChange)
	return sub, dropped, nil
}

// ResetSubscriptions puts scope's subscriptions back as start-up left them
// -- the default environment's seeded ones; a customer had none -- and
// nothing else: payments, balances and the notifications of the
// subscriptions it keeps are untouched. It is the subscription half of a
// reset of scope all (Bank.ResetIn), for a lab that has to start a run
// from no subscriptions but the seed without losing what the bank holds.
//
// It counts, by kind, the subscriptions it changed (as a reset does: each
// one deleted, and each seeded one put back because it had been deleted or
// written to since), the notifications that went with the deleted ones,
// and their pauses.
func (b *Bank) ResetSubscriptions(scope string) map[string]int {
	scope = tenant.Normalize(scope)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disp.mu.Lock()
	b.subs.mu.Lock()
	before := map[string]bool{}
	for id, sub := range b.subs.subs {
		if tenant.InScope(scope, sub.Env) {
			before[id] = true
		}
	}
	cleared := map[string]int{"subscriptions": b.subs.restoreLocked(scope, b.seed)}
	notifications, pauses := 0, 0
	for id := range before {
		if _, kept := b.subs.subs[id]; kept {
			continue
		}
		n, paused := b.disp.forgetLocked(id)
		notifications += n
		if paused {
			pauses++
		}
	}
	cleared["notifications"], cleared["pauses"] = notifications, pauses
	b.subs.mu.Unlock()
	b.disp.mu.Unlock()
	changed(b.subs.OnChange)
	changed(b.disp.OnChange)
	return cleared
}

// deleteLocked removes subscription id and its events. Caller holds s.mu.
func (s *SubscriptionStore) deleteLocked(id string) {
	if sub, ok := s.subs[id]; ok {
		for _, e := range sub.Events {
			delete(s.events, e.ID)
		}
	}
	delete(s.subs, id)
}

// forgetLocked drops what the dispatcher holds for subscription id: its
// batch filling up (and the flush timer), what a pause parked and the
// pause, and what it retained. It counts the notifications dropped and
// reports whether it was paused. Caller holds d.mu.
func (d *Dispatcher) forgetLocked(id string) (notifications int, paused bool) {
	if q, ok := d.queues[id]; ok {
		notifications += len(q.items)
		if q.timer != nil {
			q.timer.Stop()
		}
		delete(d.queues, id)
	}
	notifications += len(d.pending[id]) + len(d.queued[id])
	delete(d.pending, id)
	delete(d.queued, id)
	paused = d.paused[id]
	delete(d.paused, id)
	delete(d.owners, id)
	return notifications, paused
}
