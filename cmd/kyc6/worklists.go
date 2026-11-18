package main

// Ongoing monitoring: the worklists a platform creates, the businesses and
// individuals it enrols on them, the matches KYC6's re-screening finds
// against those records, and the reviews the platform sends back to close
// them. Each environment has its own worklists.
//
// KYC6 finds matches on its own schedule; the lab cannot, so a test puts one
// on a record at POST /sim/kyc6/worklists/{worklistId}/{kind}/{recordId}/matches
// and reads what the platform sent back at GET /sim/kyc6/worklists/….
// docs/api/services/kyc6.md says which parts of this are KYC6's documented
// contract and which are the lab's reading of it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

const (
	kindBusinesses  = "businesses"
	kindIndividuals = "individuals"

	matchOpen          = "open"
	matchTruePositive  = "true-positive"
	matchFalsePositive = "false-positive"

	recordOpen   = "open"
	recordClosed = "closed"

	endpointReviews     = "reviews"
	endpointAutoReviews = "auto-reviews"

	// reviewExistsCode is KYC6's refusal of an auto-review for a match that
	// already holds a review.
	reviewExistsCode    = "MAPI-312"
	reviewExistsMessage = "Auto review already exists"

	defaultMatchScore = 92
)

var defaultMatchDatasets = []string{"SAN-CURRENT"}

var recordKinds = []string{kindBusinesses, kindIndividuals}

// monitorEnv is one environment's ongoing monitoring.
type monitorEnv struct {
	worklists    map[string]*worklist
	matchesArmed int
}

type worklist struct {
	id       string
	settings map[string]any
	created  time.Time
	updated  time.Time
	records  map[string]map[string]*record
}

type record struct {
	id        string
	kind      string
	name      string
	dob       string
	addresses []map[string]any
	created   time.Time
	updated   time.Time
	matches   []*match
	// autoReviews are the auto-reviews the record holds, by qrCode: one
	// closes the match with that qrCode now, or as soon as it appears.
	autoReviews map[string]*review
	// reviews is every review entry sent for the record, in order, with the
	// answer it was given.
	reviews []*review
}

type match struct {
	id         string
	resourceID string
	qrCode     string
	name       string
	score      float64
	datasets   []string
	countries  []string
	version    int
	status     string
	lastReview *review
	created    time.Time
}

type review struct {
	Endpoint      string       `json:"endpoint"`
	QRCode        string       `json:"qrCode"`
	ReferenceID   string       `json:"referenceId"`
	Status        string       `json:"status"`
	ClientComment string       `json:"clientComment"`
	ClientUserID  string       `json:"clientUserId"`
	MatchVersion  any          `json:"matchVersion"`
	At            time.Time    `json:"at"`
	StatusCode    int          `json:"statusCode"`
	Error         *reviewError `json:"error,omitempty"`
}

type reviewError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// routeMonitoring serves the worklist routes. Each is written out whole, so
// the API reference test finds every one.
func (a *app) routeMonitoring() {
	sim := a.sim
	sim.Vendor("PUT /kyc6/worklists/{worklistId}", sim.APIKey(a.putWorklist))
	sim.Vendor("GET /kyc6/worklists/{worklistId}", sim.APIKey(a.getWorklist))
	sim.Vendor("DELETE /kyc6/worklists/{worklistId}", sim.APIKey(a.deleteWorklist))
	sim.Vendor("GET /kyc6/worklists/{worklistId}/matches", sim.APIKey(a.worklistMatches))

	sim.Vendor("GET /kyc6/worklists/{worklistId}/businesses", sim.APIKey(a.listRecords(kindBusinesses)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/businesses/{recordId}", sim.APIKey(a.putRecord(kindBusinesses)))
	sim.Vendor("DELETE /kyc6/worklists/{worklistId}/businesses/{recordId}", sim.APIKey(a.deleteRecord(kindBusinesses)))
	sim.Vendor("GET /kyc6/worklists/{worklistId}/businesses/{recordId}/matches", sim.APIKey(a.recordMatches(kindBusinesses)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/businesses/{recordId}/matches/reviews", sim.APIKey(a.reviewMatches(kindBusinesses, endpointReviews)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/businesses/{recordId}/matches/auto-reviews", sim.APIKey(a.reviewMatches(kindBusinesses, endpointAutoReviews)))

	sim.Vendor("GET /kyc6/worklists/{worklistId}/individuals", sim.APIKey(a.listRecords(kindIndividuals)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/individuals/{recordId}", sim.APIKey(a.putRecord(kindIndividuals)))
	sim.Vendor("DELETE /kyc6/worklists/{worklistId}/individuals/{recordId}", sim.APIKey(a.deleteRecord(kindIndividuals)))
	sim.Vendor("GET /kyc6/worklists/{worklistId}/individuals/{recordId}/matches", sim.APIKey(a.recordMatches(kindIndividuals)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/individuals/{recordId}/matches/reviews", sim.APIKey(a.reviewMatches(kindIndividuals, endpointReviews)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/individuals/{recordId}/matches/auto-reviews", sim.APIKey(a.reviewMatches(kindIndividuals, endpointAutoReviews)))

	sim.Control("GET /sim/kyc6/worklists", a.readWorklists)
	sim.Control("GET /sim/kyc6/worklists/{worklistId}/businesses/{recordId}", a.readRecord(kindBusinesses))
	sim.Control("POST /sim/kyc6/worklists/{worklistId}/businesses/{recordId}/matches", a.armMatch(kindBusinesses))
	sim.Control("GET /sim/kyc6/worklists/{worklistId}/individuals/{recordId}", a.readRecord(kindIndividuals))
	sim.Control("POST /sim/kyc6/worklists/{worklistId}/individuals/{recordId}/matches", a.armMatch(kindIndividuals))
}

// --- state --------------------------------------------------------------

func (a *app) monitorIn(env string) *monitorEnv {
	env = tenant.Normalize(env)
	m := a.monitoring[env]
	if m == nil {
		m = &monitorEnv{worklists: map[string]*worklist{}}
		a.monitoring[env] = m
	}
	return m
}

// findRecord is the caller's record, or the answer that it is not there.
// The caller holds a.mu.
func (a *app) findRecord(env, worklistID, kind, recordID string) (*worklist, *record, string) {
	wl := a.monitorIn(env).worklists[worklistID]
	if wl == nil {
		return nil, nil, "no worklist " + worklistID
	}
	rec := wl.records[kind][recordID]
	if rec == nil {
		return wl, nil, "no " + singular(kind) + " " + recordID + " on worklist " + worklistID
	}
	return wl, rec, ""
}

func singular(kind string) string {
	if kind == kindBusinesses {
		return "business"
	}
	return "individual"
}

// resetMonitoring is the flow reset's share: the records, and with them
// their matches and reviews. The worklists stay, as the platform creates one
// once per customer and would not create it again; all clears them too.
func (a *app) resetMonitoring(scope string, all bool) map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	cleared := map[string]int{"records": 0, "matches": 0, "reviews": 0}
	worklists := 0
	for env, m := range a.monitoring {
		if !tenant.InScope(scope, env) {
			continue
		}
		for _, wl := range m.worklists {
			for _, kind := range recordKinds {
				for _, rec := range wl.records[kind] {
					cleared["records"]++
					cleared["matches"] += len(rec.matches)
					cleared["reviews"] += len(rec.reviews)
				}
			}
			wl.records = emptyRecords()
		}
		m.matchesArmed = 0
		if all {
			worklists += len(m.worklists)
			m.worklists = map[string]*worklist{}
		}
	}
	if all {
		cleared["worklists"] = worklists
	}
	return cleared
}

func emptyRecords() map[string]map[string]*record {
	return map[string]map[string]*record{kindBusinesses: {}, kindIndividuals: {}}
}

// --- worklists ----------------------------------------------------------

// putWorklist creates a worklist, or replaces the settings of one that
// exists and keeps its records.
func (a *app) putWorklist(w http.ResponseWriter, r *http.Request) {
	var settings map[string]any
	_ = json.NewDecoder(r.Body).Decode(&settings)
	if settings == nil {
		settings = map[string]any{}
	}
	id := r.PathValue("worklistId")
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.monitorIn(tenant.Of(r.Context()))
	wl := m.worklists[id]
	if wl == nil {
		wl = &worklist{id: id, created: now, records: emptyRecords()}
		m.worklists[id] = wl
	}
	wl.settings = settings
	wl.updated = now
	out := wl.view()
	out["worklistId"] = id
	httputilx.WriteJSON(w, 200, out)
}

func (a *app) getWorklist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("worklistId")
	a.mu.Lock()
	defer a.mu.Unlock()
	wl := a.monitorIn(tenant.Of(r.Context())).worklists[id]
	if wl == nil {
		httputilx.Error(w, 404, "no worklist "+id)
		return
	}
	httputilx.WriteJSON(w, 200, wl.view())
}

func (a *app) deleteWorklist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("worklistId")
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.monitorIn(tenant.Of(r.Context()))
	if m.worklists[id] == nil {
		httputilx.Error(w, 404, "no worklist "+id)
		return
	}
	delete(m.worklists, id)
	w.WriteHeader(http.StatusNoContent)
}

// view is the worklist as KYC6 describes it: its settings as they were
// sent, and the counts of what is on it.
func (wl *worklist) view() map[string]any {
	out := map[string]any{}
	for k, v := range wl.settings {
		out[k] = v
	}
	out["id"] = wl.id
	out["createdDateTimeISO"] = iso(wl.created)
	out["lastUpdatedDateTimeISO"] = iso(wl.updated)
	out["businessMonitorRecordsCount"] = len(wl.records[kindBusinesses])
	out["individualMonitorRecordsCount"] = len(wl.records[kindIndividuals])
	out["businessOpenMatchesCount"] = openMatches(wl.records[kindBusinesses])
	out["individualOpenMatchesCount"] = openMatches(wl.records[kindIndividuals])
	return out
}

func openMatches(records map[string]*record) int {
	n := 0
	for _, rec := range records {
		n += rec.count(matchOpen)
	}
	return n
}

// worklistMatches is every open match on the worklist, businesses' and
// individuals' alike, in the order they were found.
func (a *app) worklistMatches(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("worklistId")
	a.mu.Lock()
	defer a.mu.Unlock()
	wl := a.monitorIn(tenant.Of(r.Context())).worklists[id]
	if wl == nil {
		httputilx.Error(w, 404, "no worklist "+id)
		return
	}
	type found struct {
		rec *record
		m   *match
	}
	var open []found
	for _, kind := range recordKinds {
		for _, rec := range wl.records[kind] {
			for _, m := range rec.matches {
				if m.status == matchOpen {
					open = append(open, found{rec, m})
				}
			}
		}
	}
	sort.Slice(open, func(i, j int) bool { return open[i].m.created.Before(open[j].m.created) })
	matches := []map[string]any{}
	for _, f := range open {
		matches = append(matches, map[string]any{
			"matchId":    f.m.id,
			"recordId":   f.rec.id,
			"resourceId": f.m.resourceID,
			"score":      f.m.score,
			"matchName":  f.m.name,
			"datasets":   f.m.datasets,
			"status":     f.m.status,
		})
	}
	httputilx.WriteJSON(w, 200, map[string]any{"matches": matches, "totalMatches": len(matches)})
}

// --- records ------------------------------------------------------------

type recordBody struct {
	Name      string           `json:"name"`
	DOB       string           `json:"dob"`
	Addresses []map[string]any `json:"addresses"`
}

// putRecord enrols a business or an individual, or updates one already
// enrolled and keeps its matches.
func (a *app) putRecord(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body recordBody
		_ = json.NewDecoder(r.Body).Decode(&body)
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		if strings.TrimSpace(body.Name) == "" {
			httputilx.Error(w, 400, "name is required")
			return
		}
		now := time.Now().UTC()
		a.mu.Lock()
		defer a.mu.Unlock()
		wl := a.monitorIn(tenant.Of(r.Context())).worklists[worklistID]
		if wl == nil {
			httputilx.Error(w, 404, "no worklist "+worklistID)
			return
		}
		rec := wl.records[kind][recordID]
		if rec == nil {
			rec = &record{id: recordID, kind: kind, created: now, autoReviews: map[string]*review{}}
			wl.records[kind][recordID] = rec
		}
		rec.name, rec.dob, rec.addresses, rec.updated = body.Name, body.DOB, body.Addresses, now
		out := rec.view()
		out["monitorRecordId"] = recordID
		out["worklistId"] = worklistID
		httputilx.WriteJSON(w, 200, out)
	}
}

func (a *app) deleteRecord(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		a.mu.Lock()
		defer a.mu.Unlock()
		wl, _, missing := a.findRecord(tenant.Of(r.Context()), worklistID, kind, recordID)
		if missing != "" {
			httputilx.Error(w, 404, missing)
			return
		}
		delete(wl.records[kind], recordID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// listRecords is the worklist's businesses or individuals. status=open is
// the records with a match nobody has reviewed yet, status=closed the rest;
// without it, every record. The counts are the whole worklist's, whatever
// the filter.
func (a *app) listRecords(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		worklistID := r.PathValue("worklistId")
		status := r.URL.Query().Get("status")
		if status != "" && status != recordOpen && status != recordClosed {
			httputilx.Error(w, 400, fmt.Sprintf("status %q is not one of open, closed", status))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		wl := a.monitorIn(tenant.Of(r.Context())).worklists[worklistID]
		if wl == nil {
			httputilx.Error(w, 404, "no worklist "+worklistID)
			return
		}
		records := sortedRecords(wl.records[kind])
		listed := []map[string]any{}
		openRecords, open, truePositive, falsePositive := 0, 0, 0, 0
		for _, rec := range records {
			open += rec.count(matchOpen)
			truePositive += rec.count(matchTruePositive)
			falsePositive += rec.count(matchFalsePositive)
			if rec.status() == recordOpen {
				openRecords++
			}
			if status == "" || rec.status() == status {
				listed = append(listed, rec.view())
			}
		}
		httputilx.WriteJSON(w, 200, map[string]any{
			"monitorRecords":            listed,
			"monitorRecordsCount":       len(records),
			"openMonitorRecordsCount":   openRecords,
			"openMatchesCount":          open,
			"truePositiveMatchesCount":  truePositive,
			"falsePositiveMatchesCount": falsePositive,
			"discardedMatchesCount":     0,
		})
	}
}

func sortedRecords(records map[string]*record) []*record {
	out := make([]*record, 0, len(records))
	for _, rec := range records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (rec *record) count(status string) int {
	n := 0
	for _, m := range rec.matches {
		if m.status == status {
			n++
		}
	}
	return n
}

func (rec *record) status() string {
	if rec.count(matchOpen) > 0 {
		return recordOpen
	}
	return recordClosed
}

// view is the record as KYC6 lists it.
func (rec *record) view() map[string]any {
	addresses := rec.addresses
	if addresses == nil {
		addresses = []map[string]any{}
	}
	countries := []string{}
	seen := map[string]bool{}
	for _, address := range addresses {
		if geography, _ := address["geography"].(string); geography != "" && !seen[geography] {
			seen[geography] = true
			countries = append(countries, geography)
		}
	}
	openDatasets := []string{}
	seenDataset := map[string]bool{}
	for _, m := range rec.matches {
		if m.status != matchOpen {
			continue
		}
		for _, dataset := range m.datasets {
			if !seenDataset[dataset] {
				seenDataset[dataset] = true
				openDatasets = append(openDatasets, dataset)
			}
		}
	}
	out := map[string]any{
		"id":                     rec.id,
		"name":                   rec.name,
		"countries":              countries,
		"addresses":              addresses,
		"openCount":              rec.count(matchOpen),
		"truePositiveCount":      rec.count(matchTruePositive),
		"falsePositiveCount":     rec.count(matchFalsePositive),
		"discardedCount":         0,
		"status":                 rec.status(),
		"openDatasets":           openDatasets,
		"createdDateTimeISO":     iso(rec.created),
		"lastUpdatedDateTimeISO": iso(rec.updated),
	}
	if rec.dob != "" {
		out["dob"] = rec.dob
	}
	return out
}

// --- matches ------------------------------------------------------------

func (a *app) recordMatches(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, rec, missing := a.findRecord(tenant.Of(r.Context()), r.PathValue("worklistId"), kind, r.PathValue("recordId"))
		if missing != "" {
			httputilx.Error(w, 404, missing)
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{"matches": rec.matchViews()})
	}
}

func (rec *record) matchViews() []map[string]any {
	out := []map[string]any{}
	for _, m := range rec.matches {
		out = append(out, m.view())
	}
	return out
}

// view is the match as KYC6 lists it on its record.
func (m *match) view() map[string]any {
	out := map[string]any{
		"currentStatus": m.status,
		"resourceId":    m.resourceID,
		"qrCode":        m.qrCode,
		"score":         m.score,
		"match":         m.name,
		"name":          m.name,
		"datasets":      m.datasets,
		"countries":     m.countries,
		"version":       m.version,
		"matchVersion":  m.version,
	}
	if m.lastReview != nil {
		out["lastReviewDateTimeISO"] = iso(m.lastReview.At)
		out["lastReviewDescription"] = m.lastReview.ClientComment
	}
	return out
}

// matchByResource is the match on any of env's records whose resourceId is
// id, for the profile lookup. The caller holds a.mu.
func (a *app) matchByResource(env, kind, id string) *match {
	for _, wl := range a.monitorIn(env).worklists {
		for _, rec := range wl.records[kind] {
			for _, m := range rec.matches {
				if m.resourceID == id {
					return m
				}
			}
		}
	}
	return nil
}

// --- reviews ------------------------------------------------------------

// reviewMatches takes a batch of reviews (endpoint "reviews") or
// auto-reviews ("auto-reviews") for one record. KYC6 rules on each entry and
// answers 200 for the batch, with one result per entry echoing its
// referenceId.
//
// A review closes the open match with its qrCode, or replaces the review of
// one already closed. An auto-review is held by the record and closes the
// match with its qrCode now or when it appears; one for a qrCode that already
// holds a review is refused with MAPI-312.
func (a *app) reviewMatches(kind, endpoint string) http.HandlerFunc {
	field := "reviews"
	if endpoint == endpointAutoReviews {
		field = "autoReviews"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		entries, _ := body[field].([]any)
		if len(entries) == 0 {
			httputilx.Error(w, 400, field+" is required: a list of at least one entry")
			return
		}
		now := time.Now().UTC()
		a.mu.Lock()
		defer a.mu.Unlock()
		_, rec, missing := a.findRecord(tenant.Of(r.Context()), r.PathValue("worklistId"), kind, r.PathValue("recordId"))
		if missing != "" {
			httputilx.Error(w, 404, missing)
			return
		}
		results := []map[string]any{}
		hasErrors := false
		for _, entry := range entries {
			rv := newReview(endpoint, entry, now)
			rec.apply(rv)
			rec.reviews = append(rec.reviews, rv)
			result := map[string]any{"referenceId": rv.ReferenceID, "statusCode": rv.StatusCode}
			if rv.Error != nil {
				hasErrors = true
				result["error"] = rv.Error
			}
			results = append(results, result)
		}
		rec.updated = now
		httputilx.WriteJSON(w, 200, map[string]any{"hasErrors": hasErrors, "results": results})
	}
}

func newReview(endpoint string, entry any, at time.Time) *review {
	fields, _ := entry.(map[string]any)
	return &review{
		Endpoint:      endpoint,
		QRCode:        str(fields["qrCode"]),
		ReferenceID:   str(fields["referenceId"]),
		Status:        str(fields["status"]),
		ClientComment: str(fields["clientComment"]),
		ClientUserID:  str(fields["clientUserId"]),
		MatchVersion:  fields["matchVersion"],
		At:            at,
	}
}

// str is a string or a number as a string: KYC6 echoes a referenceId
// whichever it was sent as.
func str(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return ""
}

// apply rules on one review entry and records the ruling on it.
func (rec *record) apply(rv *review) {
	refuse := func(code int, vendorCode, message string) {
		rv.StatusCode = code
		rv.Error = &reviewError{Code: vendorCode, Message: message}
	}
	switch {
	case rv.QRCode == "":
		refuse(400, "", "qrCode is required")
		return
	case rv.Status != matchTruePositive && rv.Status != matchFalsePositive:
		refuse(400, "", fmt.Sprintf("status %q is not one of true-positive, false-positive", rv.Status))
		return
	}
	m := rec.matchByQRCode(rv.QRCode)
	if rv.Endpoint == endpointReviews {
		if m == nil {
			refuse(404, "", "no match with qrCode "+rv.QRCode+" on this record")
			return
		}
		m.close(rv)
		rv.StatusCode = 200
		return
	}
	if rec.autoReviews[rv.QRCode] != nil || (m != nil && m.lastReview != nil) {
		refuse(409, reviewExistsCode, reviewExistsMessage)
		return
	}
	rec.autoReviews[rv.QRCode] = rv
	if m != nil {
		m.close(rv)
	}
	rv.StatusCode = 200
}

func (rec *record) matchByQRCode(qrCode string) *match {
	for _, m := range rec.matches {
		if m.qrCode == qrCode {
			return m
		}
	}
	return nil
}

func (m *match) close(rv *review) {
	m.status = rv.Status
	m.lastReview = rv
}

// --- lab control ----------------------------------------------------------

type armBody struct {
	ResourceID string   `json:"resourceId"`
	QRCode     string   `json:"qrCode"`
	Name       string   `json:"name"`
	Score      *float64 `json:"score"`
	Datasets   []string `json:"datasets"`
	Countries  []string `json:"countries"`
}

// armMatch is POST /sim/kyc6/worklists/{worklistId}/{kind}/{recordId}/matches:
// KYC6's re-screening found this match on the record. A resourceId the
// record already has a match for is that profile changing: the match takes
// the fields given, its version moves on and it is open again.
func (a *app) armMatch(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env := tenant.FromRequest(r)
		switch {
		case env == tenant.All:
			httputilx.Error(w, 400, "a match is put on a record in one environment, not in all of them")
			return
		case !a.sim.Known(env):
			httputilx.Error(w, 404, "no environment "+env+" at kyc6")
			return
		}
		var body armBody
		if err := httputilx.ReadJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
			httputilx.Error(w, 400, err.Error())
			return
		}
		if body.Score != nil && (*body.Score < 0 || *body.Score > 100) {
			httputilx.Error(w, 400, "score is from 0 to 100")
			return
		}
		for _, dataset := range body.Datasets {
			if strings.TrimSpace(dataset) == "" {
				httputilx.Error(w, 400, "a dataset cannot be empty")
				return
			}
		}
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		now := time.Now().UTC()
		a.mu.Lock()
		defer a.mu.Unlock()
		_, rec, missing := a.findRecord(env, worklistID, kind, recordID)
		if missing != "" {
			httputilx.Error(w, 404, missing+" in environment "+env)
			return
		}
		m, created := a.monitorIn(env).arm(rec, body, now)
		code := 200
		if created {
			code = 201
		}
		log.Printf("kyc6: match %s (version %d) on %s %s, worklist %s, environment %s", m.resourceID, m.version, singular(kind), recordID, worklistID, env)
		httputilx.WriteJSON(w, code, map[string]any{
			"env": env, "worklistId": worklistID, "kind": kind, "recordId": recordID, "match": m.view(),
		})
	}
}

// arm puts body's match on rec, or moves the one it names on a version, and
// reports whether it is new. A held auto-review for its qrCode closes it.
func (m *monitorEnv) arm(rec *record, body armBody, now time.Time) (*match, bool) {
	var found *match
	if body.ResourceID != "" {
		for _, existing := range rec.matches {
			if existing.resourceID == body.ResourceID {
				found = existing
			}
		}
	}
	created := found == nil
	if created {
		m.matchesArmed++
		n := m.matchesArmed
		found = &match{
			id:         fmt.Sprintf("sim-om-match-%d", n),
			resourceID: firstNonEmpty(body.ResourceID, fmt.Sprintf("sim-om-%d", n)),
			qrCode:     firstNonEmpty(body.QRCode, fmt.Sprintf("sim-qr-%d", n)),
			name:       firstNonEmpty(body.Name, rec.name),
			score:      defaultMatchScore,
			datasets:   defaultMatchDatasets,
			countries:  []string{},
			created:    now,
		}
		rec.matches = append(rec.matches, found)
	} else {
		if body.QRCode != "" {
			found.qrCode = body.QRCode
		}
		if body.Name != "" {
			found.name = body.Name
		}
	}
	if body.Score != nil {
		found.score = *body.Score
	}
	if len(body.Datasets) > 0 {
		found.datasets = body.Datasets
	}
	if body.Countries != nil {
		found.countries = body.Countries
	}
	found.version++
	found.status = matchOpen
	found.lastReview = nil
	if held := rec.autoReviews[found.qrCode]; held != nil {
		found.close(held)
	}
	rec.updated = now
	return found, created
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// readRecord is GET /sim/kyc6/worklists/{worklistId}/{kind}/{recordId}: the
// record as KYC6 lists it, its matches, the auto-reviews it holds and every
// review entry sent for it, with the answer each was given.
func (a *app) readRecord(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env := tenant.FromRequest(r)
		switch {
		case env == tenant.All:
			httputilx.Error(w, 400, "a record is read in one environment, not in all of them")
			return
		case !a.sim.Known(env):
			httputilx.Error(w, 404, "no environment "+env+" at kyc6")
			return
		}
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		a.mu.Lock()
		defer a.mu.Unlock()
		_, rec, missing := a.findRecord(env, worklistID, kind, recordID)
		if missing != "" {
			httputilx.Error(w, 404, missing+" in environment "+env)
			return
		}
		out := rec.labView()
		out["env"], out["worklistId"], out["kind"] = env, worklistID, kind
		httputilx.WriteJSON(w, 200, out)
	}
}

func (rec *record) labView() map[string]any {
	out := rec.view()
	out["matches"] = rec.matchViews()
	autoReviews := []*review{}
	for _, rv := range rec.autoReviews {
		autoReviews = append(autoReviews, rv)
	}
	sort.Slice(autoReviews, func(i, j int) bool { return autoReviews[i].At.Before(autoReviews[j].At) })
	out["autoReviews"] = autoReviews
	reviews := rec.reviews
	if reviews == nil {
		reviews = []*review{}
	}
	out["reviews"] = reviews
	return out
}

// readWorklists is GET /sim/kyc6/worklists: every worklist in the
// environment the call names, or in every one for *, with its records as
// GET /sim/kyc6/worklists/{worklistId}/{kind}/{recordId} shows each.
func (a *app) readWorklists(w http.ResponseWriter, r *http.Request) {
	scope := tenant.FromRequest(r)
	if !a.sim.Known(scope) {
		httputilx.Error(w, 404, "no environment "+scope+" at kyc6")
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []map[string]any{}
	envs := make([]string, 0, len(a.monitoring))
	for env := range a.monitoring {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		if !tenant.InScope(scope, env) {
			continue
		}
		worklists := a.monitoring[env].worklists
		ids := make([]string, 0, len(worklists))
		for id := range worklists {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			wl := worklists[id]
			view := wl.view()
			view["env"] = env
			for _, kind := range recordKinds {
				records := []map[string]any{}
				for _, rec := range sortedRecords(wl.records[kind]) {
					records = append(records, rec.labView())
				}
				view[kind] = records
			}
			out = append(out, view)
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"env": scope, "worklists": out})
}

// summarize marks a review batch with a refused entry as a warning in the
// journal: KYC6 answers such a batch 200 all the same.
func summarize(c *activity.Call) (string, map[string]string) {
	if c.Status != 200 || !strings.HasSuffix(c.Request.URL.Path, "reviews") {
		return "", nil
	}
	var answer struct {
		HasErrors bool `json:"hasErrors"`
		Results   []struct {
			StatusCode int `json:"statusCode"`
		} `json:"results"`
	}
	if json.Unmarshal(c.RespBody, &answer) != nil || !answer.HasErrors {
		return "", nil
	}
	refused := 0
	for _, result := range answer.Results {
		if result.StatusCode < 200 || result.StatusCode > 299 {
			refused++
		}
	}
	c.Refused = fmt.Sprintf("%d of %d refused", refused, len(answer.Results))
	return "", nil
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }
