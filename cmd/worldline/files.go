package main

// Settlement files by hand (docs/lab-api/plan.md, phase 6): two lab hooks
// on the control port, in the environment the call names, that split the
// cycle in two so the lab can keep files and choose when one is delivered.
//
// render is the cycle's cut without the rest of the cycle: the file the
// environment's transactions settle in, by the cycle's own code and named
// as the cycle would name it now. Nothing is published, wired or recorded.
//
// publish is the rest of the cycle without the cut: a settlement file from
// anywhere, read with Worldline's own parser, then encrypted and signed and
// put in the environment's folders exactly as the cycle puts a file it cut,
// and with wire=true the morning lump sum wired with it over the same path.
// A platform polling its folder cannot tell the two apart, which is the
// point. What it published is the reset's to take back, as a run's files
// are (reset.go).

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// maxFileBytes bounds a file handed to publish: WORLDLINE_MAX_FILE_MB, else
// LAB_MAX_FILE_MB -- the one setting the lab API's upload bound reads too,
// so a file the lab takes is one Worldline publishes -- else 512. A real day's file is well under a megabyte, but
// testing a platform against a file of millions of transactions is one of
// the things the lab is for, so the bound is room for that, not a target.
// A file is streamed to disk and read row by row, never held whole, so the
// bound is on disk, not memory.
func maxFileBytes() int64 {
	mb, err := strconv.ParseInt(env("WORLDLINE_MAX_FILE_MB", env("LAB_MAX_FILE_MB", "512")), 10, 64)
	if err != nil || mb <= 0 {
		mb = 512
	}
	return mb << 20
}

// csvContentType is how render answers a caller that asks for the file
// itself (Accept: text/csv) rather than JSON.
const csvContentType = "text/csv; charset=utf-8"

// maxPublishName is the 255 bytes a filename may have.
const maxPublishName = 255

// keptByHand is how many published names an environment remembers for its
// reset, as the run history keeps its last 100 runs.
const keptByHand = 100

// opPublished is the activity op a file published by hand is recorded
// under, in the sftp log beside the platform's own traffic.
const opPublished = "settlement-file.published"

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func (s *sim) fileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sim/settlement-files/render", s.renderSettlementFile)
	mux.HandleFunc("POST /sim/settlement-files/publish", s.publishSettlementFile)
	mux.HandleFunc("GET /sim/sftp", s.sftpFolders)
}

// renderedFile is render's answer: the file, as plain text, and what it
// settles.
type renderedFile struct {
	Name     string   `json:"name"`
	Content  string   `json:"content"`
	Currency string   `json:"currency"`
	MIDs     []string `json:"mids"`
	Total    string   `json:"total"`
	Items    int      `json:"items"`
	Slot     string   `json:"slot"`
	FromDate string   `json:"from_date"`
	ToDate   string   `json:"to_date"`
}

// renderSettlementFile answers the file of one currency the cycle would
// cut now from the named environment's transactions: slot ER and the
// cycle's coverage day by the lab clock unless the body names others. It
// refuses "*": a file is one environment's.
func (s *sim) renderSettlementFile(w http.ResponseWriter, r *http.Request) {
	e, ok := s.one(w, r)
	if !ok {
		return
	}
	var req struct {
		Currency string `json:"currency"`
		Date     string `json:"date"`
		Slot     string `json:"slot"`
	}
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, `want {"currency": "EUR", "date": "YYYY-MM-DD", "slot": "ER"}: `+err.Error())
		return
	}
	ccy := strings.ToUpper(strings.TrimSpace(req.Currency))
	if ccy == "" {
		httputilx.Error(w, 400, "currency is required: a settlement file is one currency's")
		return
	}
	if !currencyCode.MatchString(ccy) {
		httputilx.Error(w, 400, fmt.Sprintf("currency %q is not a three-letter code", req.Currency))
		return
	}
	slot, err := worldline.SlotName(req.Slot)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	// As the cycle: the file is named for now, and settles the coverage
	// day unless the caller names one.
	at := labclock.Now().In(time.Local)
	cfg := e.acq.config()
	fromDate, toDate := cfg.CoverageFor(at)
	if req.Date != "" {
		if _, err := time.Parse("2006-01-02", req.Date); err != nil {
			httputilx.Error(w, 400, fmt.Sprintf("date %q is not YYYY-MM-DD", req.Date))
			return
		}
		fromDate, toDate = req.Date, req.Date
	}

	started := e.guard.start()
	f, write, found, err := worldline.CutCurrency(e.acq.store, cfg, slot, ccy, fromDate, toDate, at)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	// A render has no effect to hold off, but one that read the
	// transactions a reset then cleared describes a file that no longer
	// settles anything.
	if e.guard.moved(started) {
		httputilx.Error(w, 409, "worldline was reset while this file was rendered: render it again")
		return
	}
	if !found {
		httputilx.Error(w, 404, nothingSettles(e.env, cfg, slot, ccy, fromDate, toDate))
		return
	}
	if wantsCSV(r) {
		writeRendered(w, f, write)
		return
	}
	var content strings.Builder
	if err := write(&content); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, renderedFile{
		Name: f.Filename, Content: content.String(), Currency: f.Currency, MIDs: f.MIDs,
		Total: formatMinor(f.AmountCents), Items: f.Items, Slot: f.Slot, FromDate: f.FromDate, ToDate: f.ToDate,
	})
}

// wantsCSV is whether render's caller asked for the file itself.
func wantsCSV(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, part := range strings.Split(v, ",") {
			mt, _, _ := strings.Cut(strings.TrimSpace(part), ";")
			if strings.EqualFold(strings.TrimSpace(mt), "text/csv") {
				return true
			}
		}
	}
	return false
}

// writeRendered answers the file itself, streamed as it is rendered, with
// what it settles in headers: a file of millions of transactions is never
// held whole, here or by the caller. The MIDs are counted, not listed: a
// header is no place for a list of thousands.
func writeRendered(w http.ResponseWriter, f worldline.CutFile, write func(io.Writer) error) {
	h := w.Header()
	h.Set("Content-Type", csvContentType)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}))
	h.Set("Lab-File-Name", f.Filename)
	h.Set("Lab-File-Currency", f.Currency)
	h.Set("Lab-File-Total", formatMinor(f.AmountCents))
	h.Set("Lab-File-Items", strconv.Itoa(f.Items))
	h.Set("Lab-File-MIDs", strconv.Itoa(len(f.MIDs)))
	h.Set("Lab-File-Slot", f.Slot)
	h.Set("Lab-File-From-Date", f.FromDate)
	h.Set("Lab-File-To-Date", f.ToDate)
	w.WriteHeader(200)
	bw := bufio.NewWriterSize(w, 1<<16)
	err := write(bw)
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		// The status is sent: all that is left to say a file was cut
		// short is to not end it, which the caller reads as a broken body.
		log.Printf("worldline: render %s: %v", f.Filename, err)
		panic(http.ErrAbortHandler)
	}
}

// nothingSettles is render's 404: why there is no file.
func nothingSettles(env string, cfg worldline.CycleConfig, slot, ccy, fromDate, toDate string) string {
	day := fromDate
	if toDate != fromDate {
		day = fromDate + " to " + toDate
	}
	if cfg.Source == worldline.FileSourceFixture {
		return fmt.Sprintf("nothing settles in %s in %s's %s file: worldline serves fixtures, and none for the %s slot is a %s settlement file",
			ccy, tenant.Normalize(env), slot, slot, ccy)
	}
	return fmt.Sprintf("nothing settles in %s on %s in %s: worldline holds no %s transaction of that day there",
		ccy, day, tenant.Normalize(env), ccy)
}

// publishedFile is publish's answer.
type publishedFile struct {
	// Published is the name the file has in download/ and to_WLNORDIC/,
	// as a cycle run lists it.
	Published string   `json:"published"`
	Name      string   `json:"name"`
	Currency  string   `json:"currency"`
	Total     string   `json:"total"`
	MIDs      []string `json:"mids"`
	LumpSum   *LumpSum `json:"lump_sum"`
}

// publishSettlementFile publishes the settlement file in the body, plain
// text, in the named environment's folders under ?name=, as the cycle
// publishes one it cut, and with ?wire=true wires its total as the morning
// cycle does. It refuses "*": a file is one environment's.
func (s *sim) publishSettlementFile(w http.ResponseWriter, r *http.Request) {
	e, ok := s.one(w, r)
	if !ok {
		return
	}
	// Work in flight when a reset lands is dropped from here on, as a
	// cycle run's is from its start.
	started := e.guard.start()
	q := r.URL.Query()
	name := q.Get("name")
	if err := checkPublishName(name); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	wire := false
	if v := q.Get("wire"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			httputilx.Error(w, 400, fmt.Sprintf("wire=%q: want true or false", v))
			return
		}
		wire = b
	}
	// A file declared larger than the bound is refused before a byte of it
	// is read. A caller that waits for 100 Continue (the lab API does) then
	// never sends it, and reads this answer, not a connection closed under
	// a body it is still sending.
	limit := maxFileBytes()
	if r.ContentLength > limit {
		httputilx.Error(w, 413, tooLarge(limit))
		return
	}
	// The file is staged on disk and read from there, twice: once row by
	// row to check it, and once into the SFTP root's staging folder. It is
	// never held whole.
	staged, err := os.CreateTemp("", "worldline-publish-*.csv")
	if err != nil {
		httputilx.Error(w, 500, "stage the file: "+err.Error())
		return
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if _, err := io.Copy(staged, http.MaxBytesReader(w, r.Body, limit)); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httputilx.Error(w, 413, tooLarge(limit))
			return
		}
		httputilx.Error(w, 400, "read body: "+err.Error())
		return
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	sum, err := worldline.SummarizeSettlementFile(staged)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	name = delivered(name, e.acq.config().Identifier, labclock.Now())
	out, err := s.publishFile(e, started, name, staged, sum, wire)
	switch {
	case errors.Is(err, errReset):
		httputilx.Error(w, 409, "worldline was reset while this file was being published: nothing of it is left, publish it again")
		return
	case err != nil:
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 201, out)
}

// tooLarge is publish's 413.
func tooLarge(limit int64) string {
	return fmt.Sprintf("a settlement file here is at most %d MiB (WORLDLINE_MAX_FILE_MB)", limit>>20)
}

// checkPublishName refuses a name that is not one file's in the folder it
// is published to.
func checkPublishName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required: the file's name, as the cycle names its own ({YYYYMMDDHHMMSS}_{identifier}_{ER|AR}_{CCY}.csv)")
	case strings.ContainsAny(name, `/\`) || strings.Contains(name, ".."):
		return fmt.Errorf("name %q: a file name, with no path separator and no ..", name)
	case len(name) > maxPublishName:
		return fmt.Errorf("name is %d bytes, at most %d", len(name), maxPublishName)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return fmt.Errorf("name %q has a control character", name)
	}
	return nil
}

// publishFile puts the file src holds, which sum describes, in e's
// download/ and to_WLNORDIC/ under name, plain, as the cycle publishes a
// file it cut, and with wire wires its total to e's safeguarding account
// as the morning cycle does: only a file that is not an afternoon (AR)
// confirmation, and only a positive total. Each effect lands only if no
// reset came since started; the first that would land after one ends it
// with errReset, and what landed before is the reset's to take back.
func (s *sim) publishFile(e *envSim, started uint64, name string, src io.ReadSeeker, sum worldline.SettlementSummary, wire bool) (publishedFile, error) {
	ccy := strings.ToUpper(sum.Meta.SettlementCurrency)
	amount := sum.Meta.SettlementAmount
	out := publishedFile{Name: name, Currency: ccy, Total: formatMinor(amount), MIDs: append([]string{}, sum.MIDs...)}
	wire = wire && worldline.FilenameSlot(name) != worldline.SlotAfternoon && amount > 0

	// The file goes to the SFTP root's staging folder first, which takes as
	// long as its disk does; only its publish -- a rename into each folder
	// -- waits for the guard. A reset never waits on a copy of hundreds of
	// megabytes, nor does anything else of the environment's behind it.
	staged, err := wlsftp.Stage(e.acq.root, name, func(w io.Writer) error {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err := io.Copy(w, src)
		return err
	})
	if err != nil {
		return out, err
	}
	defer staged.Discard()
	if !e.guard.apply(started, func() {
		out.Published, err = staged.Commit()
		if err != nil {
			return
		}
		e.acq.publishedByHand(out.Published)
		if !wire {
			s.recordPublished(e.env, out)
		}
	}) {
		return out, errReset
	}
	if err != nil {
		return out, err
	}
	if wire {
		// The cycle's reference names the day settled, its toDate, which
		// is the file's value date.
		valueDate := sum.Meta.ValueDate
		if valueDate == "" {
			valueDate = labclock.Now().UTC().Format("2006-01-02")
		}
		if !e.guard.apply(started, func() {
			ls := e.acq.wireLumpSum(ccy, amount, valueDate)
			e.acq.wiredByHand()
			out.LumpSum = &ls
			s.recordPublished(e.env, out)
		}) {
			return out, errReset
		}
	}
	status := "none"
	if out.LumpSum != nil {
		status = out.LumpSum.Status
	}
	log.Printf("worldline: settlement file published by hand env=%s file=%s currency=%s total=%s mids=%d lump_sum=%s",
		tenant.Normalize(e.env), out.Published, ccy, out.Total, len(out.MIDs), status)
	return out, nil
}

// publishedByHand remembers a file a hook published, for the reset.
func (s *settlementApp) publishedByHand(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.byHand {
		if n == name {
			return
		}
	}
	s.byHand = append(s.byHand, name)
	if len(s.byHand) > keptByHand {
		s.byHand = s.byHand[len(s.byHand)-keptByHand:]
	}
}

// wiredByHand counts a lump sum a hook wired, for the reset.
func (s *settlementApp) wiredByHand() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handLumpSums++
}

// recordPublished puts a file published by hand in env's sftp activity,
// beside the platform's own traffic. The caller holds env's guard.
func (s *sim) recordPublished(env string, out publishedFile) {
	ev := activity.Event{
		Env: env, Op: opPublished, Status: activity.StatusOK,
		Summary: fmt.Sprintf("published %s: %s %s for %d MID(s)", out.Published, out.Currency, out.Total, len(out.MIDs)),
		Detail: map[string]string{
			"path": wlsftp.DirDownload + "/" + out.Published, "file": out.Published,
			"currency": out.Currency, "total": out.Total, "mids": strings.Join(out.MIDs, ","),
		},
	}
	if ls := out.LumpSum; ls != nil {
		ev.Summary += "; lump sum " + ls.Status
		if ls.AccountID != "" {
			ev.Summary += " to " + ls.AccountID
			ev.Detail["account_id"] = ls.AccountID
		}
		ev.Detail["lump_sum"] = ls.Status
		if ls.Status != "credited" {
			ev.Status = activity.StatusWarn
		}
	}
	s.sftpLog.Record(ev)
}

// worldlineStamp is the timestamp Worldline's own file names start with
// (worldline.BamboraFilename).
var worldlineStamp = regexp.MustCompile(`^[0-9]{14}_`)

// worldlineName is a whole name in Worldline's shape: its slot and currency.
var worldlineName = regexp.MustCompile(`^[0-9]{14}_.+_(ER|AR)_([A-Z]{3})\.csv$`)

// delivered names a file as Worldline names the files it drops: under the
// environment's identifier, at the moment it is published. A platform
// takes only its own identifier's files, and only those of its own day, so
// a file rendered yesterday, or named for another customer, must read as
// this customer's file of today. A name stamped as Worldline stamps its
// files but otherwise its own keeps the rest of it; any other name is kept.
func delivered(name, identifier string, at time.Time) string {
	if m := worldlineName.FindStringSubmatch(name); m != nil && identifier != "" {
		return worldline.BamboraFilename(identifier, at, m[1], m[2])
	}
	if !worldlineStamp.MatchString(name) {
		return name
	}
	return worldline.FileStamp(at) + name[14:]
}
