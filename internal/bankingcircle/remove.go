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
// the pause itself, what it retained after giving up on the endpoint, and
// the batches on their way out. Left behind, they would sit in the state
// file for good -- nothing can release a pause on a subscription nobody can
// name -- and a subscription put back later under the same id would inherit
// them. A batch on its way out stops at its next step, which finds its
// flight dropped (Dispatcher.deliverSync): mid-schedule, or still waiting
// its turn in a release. Only a send already on the wire finishes, and it
// is not retried.
//
// Neither moves the run, as a reset does: the subscription's work stops
// through its own flights, and the environment's payments carry on. A
// subscription made later under a deleted one's id -- a reset of the
// subscriptions hands out the seed's ids again once nothing else is left --
// inherits nothing: no flight of the old one's is left to find.

// RemoveSubscription deletes subscription id, scope's, and what the
// dispatcher held for it. ifMatch is the vendor's concurrency check: nil
// is the lab's override, which takes any rowVersion. It answers the
// subscription as it was and how many notifications went with it: every
// one it held that will now never be sent. A send on the wire at that
// moment is not among them; it may still be acknowledged.
// ErrSubscriptionNotFound for another environment's, as for an unknown one.
//
// It takes the dispatcher's and the store's locks, not the bank's: it is
// the vendor's DELETE, and must not wait behind a reset. A reset takes the
// same two locks, so a delete lands wholly before it or wholly after.
func (b *Bank) RemoveSubscription(scope, id string, ifMatch *string) (*Subscription, int, error) {
	sub, dropped, err := b.remove(scope, id, ifMatch)
	if err != nil {
		return nil, 0, err
	}
	changed(b.subs.OnChange)
	changed(b.disp.OnChange)
	return sub, dropped, nil
}

func (b *Bank) remove(scope, id string, ifMatch *string) (*Subscription, int, error) {
	b.disp.mu.Lock()
	defer b.disp.mu.Unlock()
	b.subs.mu.Lock()
	defer b.subs.mu.Unlock()
	sub, ok := b.subs.subs[id]
	if !ok || !tenant.InScope(tenant.Normalize(scope), sub.Env) {
		return nil, 0, ErrSubscriptionNotFound
	}
	if ifMatch != nil {
		if err := checkRowVersion(sub.RowVersion, *ifMatch); err != nil {
			return nil, 0, err
		}
	}
	b.subs.deleteLocked(id)
	dropped, _ := b.disp.forgetLocked(id)
	return sub, dropped, nil
}

// ResetSubscriptions puts scope's subscriptions back as start-up left them
// -- the default environment's seeded ones; a customer had none -- and
// nothing else: payments, balances and accounts are untouched. It is the
// subscription half of a reset of scope all (Bank.ResetIn), for a lab that
// has to start a run from no subscriptions but the seed without losing what
// the bank holds.
//
// A seeded subscription comes back as start-up left it: not paused, with
// nothing parked behind a pause and nothing retained after a deactivation.
// What it was about to send -- its batch filling up, its batches on their
// way out -- still goes.
//
// It counts, by kind, the subscriptions it changed (as a reset does: each
// one deleted, and each seeded one put back because it had been deleted or
// written to since), the notifications it dropped, and the pauses it
// lifted.
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
		var n int
		var paused bool
		if _, kept := b.subs.subs[id]; kept {
			n, paused = b.disp.clearHeldLocked(id)
		} else {
			n, paused = b.disp.forgetLocked(id)
		}
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

// clearHeldLocked drops what a pause and a deactivation left for
// subscription id: the pause, what was parked behind it, and what was
// retained after the retry schedule gave up. It counts the notifications
// dropped and reports whether it was paused. Caller holds d.mu.
func (d *Dispatcher) clearHeldLocked(id string) (notifications int, paused bool) {
	notifications = len(d.pending[id]) + len(d.queued[id])
	delete(d.pending, id)
	delete(d.queued, id)
	paused = d.paused[id]
	delete(d.paused, id)
	return notifications, paused
}

// forgetLocked drops everything the dispatcher holds for subscription id:
// what clearHeldLocked drops, its batch filling up (and the flush timer),
// and its batches on their way out, which stop at their next step. It
// counts the notifications it dropped -- not a batch on the wire, which
// may yet be delivered -- and reports whether it was paused. Caller holds
// d.mu.
func (d *Dispatcher) forgetLocked(id string) (notifications int, paused bool) {
	notifications, paused = d.clearHeldLocked(id)
	if q, ok := d.queues[id]; ok {
		notifications += len(q.items)
		if q.timer != nil {
			q.timer.Stop()
		}
		delete(d.queues, id)
	}
	for f := range d.inflight {
		if f.sub == id {
			if !f.sending {
				notifications += f.n
			}
			delete(d.inflight, f)
		}
	}
	delete(d.owners, id)
	return notifications, paused
}
