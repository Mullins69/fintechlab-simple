package main

import (
	"net/http"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/vendorsim/vendorsimtest"
)

const (
	worklistPath = "/kyc6/worklists/wl-7"
	companyPath  = worklistPath + "/businesses/company-42"
	ownerPath    = worklistPath + "/individuals/owner-42-3"
	armCompany   = "/sim/kyc6/worklists/wl-7/businesses/company-42/matches"
	readCompany  = "/sim/kyc6/worklists/wl-7/businesses/company-42"
)

var defaultKey = map[string]string{"x-api-key": "k"}

func surfaces(t *testing.T) (svc, ctl http.Handler) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8092")
	if err != nil {
		t.Fatal(err)
	}
	vendor, control := a.sim.Surfaces()
	return vendor, control.Handler()
}

// enrol creates the worklist and puts one business and one individual on it,
// as the platform's monitoring client does.
func enrol(t *testing.T, svc http.Handler, key map[string]string) {
	t.Helper()
	if code, out := vendorsimtest.Call(t, svc, "PUT", worklistPath, `{"name":"Partner 7","threshold":85,"datasets":["SAN","PEP"],"frequency":"daily"}`, key); code != 200 || out["worklistId"] != "wl-7" {
		t.Fatalf("create worklist: %d %v", code, out)
	}
	if code, out := vendorsimtest.Call(t, svc, "PUT", companyPath, `{"name":"Southwind Coffee Ltd","addresses":[{"geography":"GB"}]}`, key); code != 200 || out["monitorRecordId"] != "company-42" {
		t.Fatalf("add business: %d %v", code, out)
	}
	if code, out := vendorsimtest.Call(t, svc, "PUT", ownerPath, `{"name":"Ada Lovelace","addresses":[{"geography":"GB","city":"London"}],"dob":"1815-12-10"}`, key); code != 200 {
		t.Fatalf("add individual: %d %v", code, out)
	}
}

func records(t *testing.T, svc http.Handler, path string, key map[string]string) (map[string]any, []any) {
	t.Helper()
	code, out := vendorsimtest.Call(t, svc, "GET", path, "", key)
	if code != 200 {
		t.Fatalf("GET %s: %d %v", path, code, out)
	}
	list, _ := out["monitorRecords"].([]any)
	return out, list
}

func matchesOf(t *testing.T, svc http.Handler, path string, key map[string]string) []any {
	t.Helper()
	code, out := vendorsimtest.Call(t, svc, "GET", path+"/matches", "", key)
	if code != 200 {
		t.Fatalf("GET %s/matches: %d %v", path, code, out)
	}
	list, _ := out["matches"].([]any)
	return list
}

func TestAnEnrolledRecordIsListedAndClosedUntilAMatchIsPutOnIt(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)

	_, worklist := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey)
	for field, want := range map[string]any{"id": "wl-7", "name": "Partner 7", "threshold": float64(85),
		"businessMonitorRecordsCount": float64(1), "individualMonitorRecordsCount": float64(1), "businessOpenMatchesCount": float64(0)} {
		if worklist[field] != want {
			t.Errorf("worklist %s = %v, want %v", field, worklist[field], want)
		}
	}

	all, list := records(t, svc, worklistPath+"/businesses", defaultKey)
	if len(list) != 1 || all["monitorRecordsCount"] != float64(1) {
		t.Fatalf("every business: %v", all)
	}
	company := list[0].(map[string]any)
	if company["id"] != "company-42" || company["name"] != "Southwind Coffee Ltd" || company["openCount"] != float64(0) || company["status"] != "closed" {
		t.Errorf("enrolled business: %v", company)
	}
	if countries, _ := company["countries"].([]any); len(countries) != 1 || countries[0] != "GB" {
		t.Errorf("countries come from the addresses' geography: %v", company["countries"])
	}
	if _, open := records(t, svc, worklistPath+"/businesses?status=open", defaultKey); len(open) != 0 {
		t.Errorf("nothing is open before a match: %v", open)
	}
	if got := matchesOf(t, svc, companyPath, defaultKey); len(got) != 0 {
		t.Errorf("no matches before one is put there: %v", got)
	}

	code, armed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"datasets":["SAN-CURRENT","PEP-CURRENT"],"score":96}`, nil)
	if code != 201 {
		t.Fatalf("arm: %d %v", code, armed)
	}
	m := armed["match"].(map[string]any)
	if m["resourceId"] == "" || m["qrCode"] == "" || m["matchVersion"] != float64(1) || m["currentStatus"] != "open" || m["name"] != "Southwind Coffee Ltd" || m["score"] != float64(96) {
		t.Errorf("armed match: %v", m)
	}

	openList, open := records(t, svc, worklistPath+"/businesses?status=open", defaultKey)
	if len(open) != 1 || openList["openMonitorRecordsCount"] != float64(1) || openList["openMatchesCount"] != float64(1) {
		t.Fatalf("open businesses: %v", openList)
	}
	record := open[0].(map[string]any)
	if record["openCount"] != float64(1) || record["status"] != "open" {
		t.Errorf("open record: %v", record)
	}
	if datasets, _ := record["openDatasets"].([]any); len(datasets) != 2 {
		t.Errorf("openDatasets: %v", record["openDatasets"])
	}
	if _, open := records(t, svc, worklistPath+"/individuals?status=open", defaultKey); len(open) != 0 {
		t.Errorf("a business's match does not open an individual: %v", open)
	}

	got := matchesOf(t, svc, companyPath, defaultKey)
	if len(got) != 1 || got[0].(map[string]any)["resourceId"] != m["resourceId"] || got[0].(map[string]any)["qrCode"] != m["qrCode"] {
		t.Fatalf("record matches: %v, want the armed one", got)
	}

	_, profile := vendorsimtest.Call(t, svc, "GET", "/kyc6/businesses/"+m["resourceId"].(string), "", defaultKey)
	if profile["name"] != "Southwind Coffee Ltd" || profile["qrCode"] != m["qrCode"] || profile["version"] != float64(1) {
		t.Errorf("the match's profile: %v", profile)
	}

	_, poll := vendorsimtest.Call(t, svc, "GET", worklistPath+"/matches", "", defaultKey)
	polled, _ := poll["matches"].([]any)
	if len(polled) != 1 || poll["totalMatches"] != float64(1) || polled[0].(map[string]any)["recordId"] != "company-42" {
		t.Errorf("worklist matches: %v", poll)
	}

	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath+"/businesses?status=maybe", "", defaultKey); code != 400 {
		t.Errorf("an unknown status: %d, want 400", code)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", "/sim/kyc6/worklists/wl-7/businesses/nobody/matches", `{}`, nil); code != 404 {
		t.Errorf("arming on a record nobody enrolled: %d, want 404", code)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"colour":"red"}`, nil); code != 400 {
		t.Errorf("an unknown field: %d, want 400", code)
	}
}

func TestAReviewClosesTheMatchAndTheRecordStopsBeingOpen(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	_, armed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil)
	qrCode := vendorsimtest.At(armed, "match.qrCode").(string)

	code, out := vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", `{"reviews":[
		{"qrCode":"`+qrCode+`","referenceId":"11","status":"false-positive","clientComment":"different person","clientUserId":"ada_lovelace_7","matchVersion":1},
		{"qrCode":"nope","referenceId":"12","status":"true-positive","clientComment":"","clientUserId":"ada_lovelace_7","matchVersion":1}]}`, defaultKey)
	if code != 200 || out["hasErrors"] != true {
		t.Fatalf("reviews: %d %v", code, out)
	}
	results := out["results"].([]any)
	first, second := results[0].(map[string]any), results[1].(map[string]any)
	if first["referenceId"] != "11" || first["statusCode"] != float64(200) || first["error"] != nil {
		t.Errorf("the review of the match: %v", first)
	}
	if second["referenceId"] != "12" || second["statusCode"] != float64(404) {
		t.Errorf("a review of a qrCode the record has no match for: %v", second)
	}

	m := matchesOf(t, svc, companyPath, defaultKey)[0].(map[string]any)
	if m["currentStatus"] != "false-positive" || m["lastReviewDescription"] != "different person" || m["lastReviewDateTimeISO"] == nil {
		t.Errorf("the reviewed match: %v", m)
	}
	if _, open := records(t, svc, worklistPath+"/businesses?status=open", defaultKey); len(open) != 0 {
		t.Errorf("a reviewed record is not open: %v", open)
	}
	all, _ := records(t, svc, worklistPath+"/businesses", defaultKey)
	if all["falsePositiveMatchesCount"] != float64(1) || all["openMatchesCount"] != float64(0) {
		t.Errorf("counts after the review: %v", all)
	}

	_, record := vendorsimtest.Call(t, ctl, "GET", readCompany, "", nil)
	reviews, _ := record["reviews"].([]any)
	if len(reviews) != 2 {
		t.Fatalf("the lab's read of the reviews: %v", record)
	}
	sent := reviews[0].(map[string]any)
	if sent["endpoint"] != "reviews" || sent["clientUserId"] != "ada_lovelace_7" || sent["matchVersion"] != float64(1) || sent["statusCode"] != float64(200) {
		t.Errorf("the review as sent: %v", sent)
	}

	// The profile changes: the match is open again on its next version.
	_, rearmed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"resourceId":"`+m["resourceId"].(string)+`","datasets":["SAN-FORMER"]}`, nil)
	if vendorsimtest.At(rearmed, "match.matchVersion") != float64(2) || vendorsimtest.At(rearmed, "match.currentStatus") != "open" {
		t.Errorf("re-armed: %v", rearmed)
	}
	if got := matchesOf(t, svc, companyPath, defaultKey); len(got) != 1 {
		t.Errorf("re-arming moves the match on rather than adding one: %v", got)
	}

	_, activity := vendorsimtest.Call(t, ctl, "GET", "/sim/activity", "", nil)
	if !journalHas(activity, "PUT "+companyPath+"/matches/reviews — 1 of 2 refused") {
		t.Errorf("the journal marks the refused entry: %v", activity)
	}
}

func journalHas(activity map[string]any, summary string) bool {
	logs, _ := activity["logs"].([]any)
	for _, l := range logs {
		events, _ := l.(map[string]any)["events"].([]any)
		for _, e := range events {
			if e.(map[string]any)["summary"] == summary {
				return true
			}
		}
	}
	return false
}

func TestAnAutoReviewClosesItsMatchWhenItAppearsAndCannotBeReplaced(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	body := `{"autoReviews":[{"qrCode":"qr-ada","referenceId":"21","status":"false-positive","clientComment":"cleared at onboarding","clientUserId":"ada_lovelace_7","matchVersion":3}]}`

	code, out := vendorsimtest.Call(t, svc, "PUT", ownerPath+"/matches/auto-reviews", body, defaultKey)
	if code != 200 || out["hasErrors"] != false || vendorsimtest.At(out, "results") == nil {
		t.Fatalf("auto-reviews: %d %v", code, out)
	}
	_, again := vendorsimtest.Call(t, svc, "PUT", ownerPath+"/matches/auto-reviews", body, defaultKey)
	result := again["results"].([]any)[0].(map[string]any)
	if again["hasErrors"] != true || vendorsimtest.At(result, "error.code") != "MAPI-312" || result["referenceId"] != "21" {
		t.Errorf("the same auto-review again: %v, want MAPI-312", again)
	}

	_, armed := vendorsimtest.Call(t, ctl, "POST", "/sim/kyc6/worklists/wl-7/individuals/owner-42-3/matches", `{"qrCode":"qr-ada"}`, nil)
	if vendorsimtest.At(armed, "match.currentStatus") != "false-positive" {
		t.Errorf("a match with an auto-reviewed qrCode arrives closed: %v", armed)
	}
	if _, open := records(t, svc, worklistPath+"/individuals?status=open", defaultKey); len(open) != 0 {
		t.Errorf("an auto-reviewed record is not open: %v", open)
	}
}

func TestEachEnvironmentHasItsOwnWorklists(t *testing.T) {
	svc, ctl := surfaces(t)
	vendorsimtest.Call(t, ctl, "PUT", "/sim/customers/sbx", `{"env":"sbx","verification":{"username":"sbx-user","password":"sbx-key"}}`, nil)
	sbxKey := map[string]string{"x-api-key": "sbx-key"}
	enrol(t, svc, sbxKey)

	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey); code != 404 {
		t.Errorf("the default environment reads sbx's worklist: %d, want 404", code)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil); code != 404 {
		t.Errorf("arming in the default environment on sbx's record: %d, want 404", code)
	}
	if code, out := vendorsimtest.Call(t, ctl, "POST", armCompany+"?env=sbx", `{}`, nil); code != 201 {
		t.Fatalf("arming in sbx: %d %v", code, out)
	}
	if _, open := records(t, svc, worklistPath+"/businesses?status=open", sbxKey); len(open) != 1 {
		t.Errorf("sbx's open records: %v", open)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", armCompany+"?env=*", `{}`, nil); code != 400 {
		t.Errorf("arming in every environment: %d, want 400", code)
	}

	_, every := vendorsimtest.Call(t, ctl, "GET", "/sim/kyc6/worklists?env=*", "", nil)
	if worklists, _ := every["worklists"].([]any); len(worklists) != 1 || worklists[0].(map[string]any)["env"] != "sbx" {
		t.Errorf("every environment's worklists: %v", every)
	}

	vendorsimtest.Call(t, ctl, "DELETE", "/sim/customers/sbx", "", nil)
	_, every = vendorsimtest.Call(t, ctl, "GET", "/sim/kyc6/worklists?env=*", "", nil)
	if worklists, _ := every["worklists"].([]any); len(worklists) != 0 {
		t.Errorf("a deleted customer's worklists stay: %v", every)
	}
}

func TestAFlowResetClearsTheRecordsAndKeepsTheWorklist(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil)
	vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"screening","outcome":"hit"}`, nil)

	_, out := vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"flow"}`, nil)
	for key, want := range map[string]float64{"records": 2, "matches": 1, "outcomes": 1} {
		if vendorsimtest.At(out, "cleared."+key) != want {
			t.Errorf("flow reset cleared %s = %v, want %v (%v)", key, vendorsimtest.At(out, "cleared."+key), want, out)
		}
	}
	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey); code != 200 {
		t.Errorf("the worklist after a flow reset: %d, want kept", code)
	}
	if _, list := records(t, svc, worklistPath+"/businesses", defaultKey); len(list) != 0 {
		t.Errorf("records after a flow reset: %v", list)
	}
	if code, _ := vendorsimtest.Call(t, svc, "GET", companyPath+"/matches", "", defaultKey); code != 404 {
		t.Errorf("a cleared record's matches: %d, want 404", code)
	}
	if _, screen := vendorsimtest.Call(t, svc, "POST", "/kyc6/businesses", `{"name":"Ada"}`, defaultKey); vendorsimtest.At(screen, "results.matchCount") != float64(0) {
		t.Errorf("the screening outcome after a reset: %v", screen)
	}

	_, out = vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"all"}`, nil)
	if vendorsimtest.At(out, "cleared.worklists") != float64(1) {
		t.Errorf("all reset cleared %v, want the worklist", out["cleared"])
	}
	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey); code != 404 {
		t.Errorf("the worklist after an all reset: %d, want 404", code)
	}
}

func TestRemovingARecordAndAWorklist(t *testing.T) {
	svc, _ := surfaces(t)
	enrol(t, svc, defaultKey)
	if code, _ := vendorsimtest.Call(t, svc, "DELETE", ownerPath, "", defaultKey); code != 204 {
		t.Errorf("remove individual: %d, want 204", code)
	}
	if _, list := records(t, svc, worklistPath+"/individuals", defaultKey); len(list) != 0 {
		t.Errorf("individuals after removal: %v", list)
	}
	if code, _ := vendorsimtest.Call(t, svc, "DELETE", ownerPath, "", defaultKey); code != 404 {
		t.Errorf("removing it twice: %d, want 404", code)
	}
	if code, _ := vendorsimtest.Call(t, svc, "DELETE", worklistPath, "", defaultKey); code != 204 {
		t.Errorf("delete worklist: %d, want 204", code)
	}
	if code, _ := vendorsimtest.Call(t, svc, "PUT", companyPath, `{"name":"x"}`, defaultKey); code != 404 {
		t.Errorf("enrolling on a deleted worklist: %d, want 404", code)
	}
}
