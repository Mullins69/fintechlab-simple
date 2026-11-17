package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// The lab's delete and the subscriptions reset (subscriptions.go): a
// subscription a run made outlives a restart now, so the lab removes it,
// and what it removes stays removed across one.
func TestTheLabDeletesASubscriptionAndItStaysGoneAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var seeded *bankingcircle.Subscription
	a := openTestApp(t, path, time.Hour, time.Hour, func(a *app) error {
		seeded = subscribeAll(t, a, "https://example.test/seeded")
		return nil
	})
	r := rigFor(t, a)
	run := subscribeAll(t, a, "https://example.test/a-run")
	a.dispatch.Pause(run.ID)

	out := mustCall(t, 200, "DELETE", r.ctl+"/sim/subscription/"+run.ID, "", "")
	if out["subscriptionId"] != run.ID || out["deleted"] != true {
		t.Errorf("the lab's delete answered %v", out)
	}
	mustCall(t, 404, "DELETE", r.ctl+"/sim/subscription/"+run.ID, "", "")
	if a.dispatch.Paused(run.ID) {
		t.Error("the deleted subscription's pause is still held")
	}
	var logged bool
	for _, e := range a.notifLog.Recent(10) {
		logged = logged || (e.Op == "subscription.delete" && strings.Contains(e.Summary, "deleted by the lab"))
	}
	if !logged {
		t.Error("the notifications log does not say the subscription was deleted")
	}

	// Another environment's delete cannot reach it; the vendor's own
	// delete still wants the rowVersion.
	mustCall(t, 404, "DELETE", r.ctl+"/sim/subscription/"+seeded.ID+"?env=sbx", "", "")
	tok := a.tokens.issue(time.Hour)
	if code, _ := call(t, "DELETE", r.vendor+"/api/v1/notificationselfservice/subscription/"+seeded.ID, tok, ""); code != 412 {
		t.Errorf("the vendor's delete without If-Match = %d, want 412", code)
	}

	// Two more runs' subscriptions, and the reset puts back the seed alone.
	subscribeAll(t, a, "https://example.test/run-2")
	subscribeAll(t, a, "https://example.test/run-3")
	reset := mustCall(t, 200, "POST", r.ctl+"/sim/subscriptions/reset", "", "")
	if cleared := reset["cleared"].(map[string]any); cleared["subscriptions"] != float64(2) || reset["env"] != tenant.Default {
		t.Errorf("the reset answered %v, want the default environment's two runs' subscriptions cleared", reset)
	}

	a.stop()
	b := openTestApp(t, path, time.Hour, time.Hour, nil)
	subs, _ := b.subs.ListIn(tenant.All, 1, 100)
	if len(subs) != 1 || subs[0].ID != seeded.ID {
		t.Fatalf("after the restart the bank holds %v, want the seeded subscription alone", subs)
	}
}

// The vendor's own DELETE is on disk before it answers, as the lab's is: a
// platform told 200 must not see its subscription back after a restart that
// came before the state file's next coalesced write.
func TestTheSubscribersDeleteIsWrittenBeforeItAnswers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a := openTestApp(t, path, time.Hour, time.Hour, nil)
	r := rigFor(t, a)
	sub := subscribeAll(t, a, "https://example.test/platform")
	if err := a.persistNow(); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("DELETE", r.vendor+"/api/v1/notificationselfservice/subscription/"+sub.ID, nil)
	req.Header.Set("Authorization", "Bearer "+a.tokens.issue(time.Hour))
	req.Header.Set("If-Match", sub.RowVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("the vendor's delete = %d, want 200", resp.StatusCode)
	}

	// Read at once: the coalesced writer waits stateWriteDelay first.
	st, err := bankingcircle.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range st.Subscriptions.Subscriptions {
		if rec.ID == sub.ID {
			t.Fatal("the state file still holds the subscription its subscriber deleted")
		}
	}
}
