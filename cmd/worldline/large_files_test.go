package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// renderCSV asks render for the file itself and answers the recorder.
func renderCSV(t *testing.T, h http.Handler, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/sim/settlement-files/render"+query, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/csv")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Asked for text/csv, render answers the file itself, the same bytes its
// JSON answer carries, with what it settles in headers.
func TestRenderAnswersTheFileItselfWhenAskedForCSV(t *testing.T) {
	w := newTestSim(t, "")
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	seedTwo(t, h)

	asJSON := render(t, h, "?env=sbx", `{"currency":"EUR","date":"`+settleDay+`"}`)
	r := renderCSV(t, h, "?env=sbx", `{"currency":"EUR","date":"`+settleDay+`"}`)
	if r.Code != 200 {
		t.Fatalf("render as text/csv: %d %s", r.Code, r.Body)
	}
	if got := r.Body.String(); got != asJSON.Content {
		t.Errorf("render as text/csv answered\n%s\nas JSON\n%s", got, asJSON.Content)
	}
	for header, want := range map[string]string{
		"Content-Type":       csvContentType,
		"Lab-File-Name":      asJSON.Name,
		"Lab-File-Currency":  "EUR",
		"Lab-File-Total":     asJSON.Total,
		"Lab-File-Items":     strconv.Itoa(asJSON.Items),
		"Lab-File-MIDs":      strconv.Itoa(len(asJSON.MIDs)),
		"Lab-File-Slot":      asJSON.Slot,
		"Lab-File-From-Date": settleDay,
		"Lab-File-To-Date":   settleDay,
	} {
		if got := r.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// A refusal is still JSON, whatever was asked for.
	if r := renderCSV(t, h, "?env=sbx", `{"currency":"USD","date":"`+settleDay+`"}`); r.Code != 404 || !strings.Contains(r.Body.String(), `"error"`) {
		t.Errorf("render of nothing as text/csv: %d %s, want the JSON 404", r.Code, r.Body)
	}
}

// A run publishes its files, written as they are rendered, and keeps what
// they settled, not their bytes: the history holds 100 runs.
func TestARunKeepsNoFileBytes(t *testing.T) {
	bc, _ := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	if _, err := w.acq.store.Add(worldline.Transaction{MID: testMerchant, Currency: "EUR", AmountCents: 10000, Date: settleDay}); err != nil {
		t.Fatal(err)
	}
	run := w.acq.run(worldline.SlotMorning, time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC), settleDay, settleDay)
	if len(run.Published) != 1 || len(run.Files) != 1 {
		t.Fatalf("run: %+v", run)
	}
	raw, err := os.ReadFile(filepath.Join(w.acq.root, wlsftp.DirDownload, run.Published[0]))
	if err != nil || len(raw) == 0 {
		t.Fatalf("the published file: %d bytes, %v", len(raw), err)
	}
	// The history's CutFile has no bytes to keep (worldline.CutFile); and
	// the file went to disk by way of the staging folder, leaving nothing
	// there.
	if got := names(t, w.acq.root, wlsftp.DirStaging); len(got) != 0 {
		t.Errorf("a run left %v staged", got)
	}
}

// A file over WORLDLINE_MAX_FILE_MB is refused before anything of it is
// published, and says which setting it is over.
func TestPublishOverTheLimitIsRefused(t *testing.T) {
	t.Setenv("WORLDLINE_MAX_FILE_MB", "1")
	bc, wires := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()

	r := serve(t, h, "POST", publishQuery("", "big.csv", "true"), strings.Repeat("x", 1<<20+1))
	if r.Code != 413 || !strings.Contains(r.Body.String(), "at most 1 MiB (WORLDLINE_MAX_FILE_MB)") {
		t.Errorf("publish over the limit: %d %s, want 413 naming WORLDLINE_MAX_FILE_MB", r.Code, r.Body)
	}
	for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
		if got := names(t, w.acq.root, d); len(got) != 0 {
			t.Errorf("a refused publish left %v in %s", got, d)
		}
	}
	if n := wires.Load(); n != 0 {
		t.Errorf("a refused publish wired %d lump sum(s)", n)
	}
}

// overOldCap is how many transactions make a file over the 64 MiB publish
// used to refuse (about 338 bytes a transaction).
const overOldCap = 210_000

// A file over the 64 MiB publish used to refuse goes the whole way through
// Worldline over HTTP: cut from its own trading and answered as itself,
// then published from that, whole, in both folders, with nothing partial
// left behind. Each side streams it, which is what lets it through.
func TestAFileOverTheOld64MiBIsCutAndPublished(t *testing.T) {
	if testing.Short() {
		t.Skip("cuts and publishes a file of over 64 MiB")
	}
	bc, _ := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	srv := httptest.NewServer(ctl.Handler())
	t.Cleanup(srv.Close)
	for i := 0; i < overOldCap; i++ {
		if _, err := w.acq.store.Add(worldline.Transaction{
			MID: fmt.Sprintf("MID-%03d", i%200), Currency: "EUR", AmountCents: int64(1000 + i%5000), Date: settleDay,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Cut: the file itself, streamed to disk.
	req, _ := http.NewRequest("POST", srv.URL+"/sim/settlement-files/render",
		strings.NewReader(`{"currency":"EUR","date":"`+settleDay+`"}`))
	req.Header.Set("Accept", "text/csv")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("render: %d %s", resp.StatusCode, b)
	}
	cut := filepath.Join(t.TempDir(), resp.Header.Get("Lab-File-Name"))
	f, err := os.Create(cut)
	if err != nil {
		t.Fatal(err)
	}
	size, err := io.Copy(f, resp.Body)
	resp.Body.Close()
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if size <= 64<<20 {
		t.Fatalf("the cut is %d bytes: not over the old 64 MiB, so it tests nothing", size)
	}
	if got := resp.Header.Get("Lab-File-Items"); got != strconv.Itoa(overOldCap) {
		t.Errorf("Lab-File-Items = %s, want %d", got, overOldCap)
	}

	// Publish: from the cut on disk.
	body, err := os.Open(cut)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	resp, err = http.Post(srv.URL+publishQuery("", filepath.Base(cut), "false"), "text/csv", body)
	if err != nil {
		t.Fatal(err)
	}
	var out publishedFile
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("publish %d bytes: %d %s", size, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Currency != "EUR" || len(out.MIDs) != 200 {
		t.Errorf("publish answered %s %d MIDs, want EUR and 200", out.Currency, len(out.MIDs))
	}
	for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
		got := names(t, w.acq.root, d)
		if len(got) != 1 || got[0] != out.Published {
			t.Errorf("%s holds %v, want only %s", d, got, out.Published)
			continue
		}
		st, err := os.Stat(filepath.Join(w.acq.root, d, out.Published))
		if err != nil || st.Size() != size {
			t.Errorf("%s/%s: %v, want %d bytes", d, out.Published, err, size)
		}
	}
}

// heldReader serves r once released, and says when it is first read: a
// file coming off a slow disk, or a slow caller, mid-copy.
type heldReader struct {
	r        *strings.Reader
	reading  chan struct{}
	release  chan struct{}
	signaled bool
}

func (h *heldReader) Read(p []byte) (int, error) {
	if !h.signaled {
		h.signaled = true
		close(h.reading)
		<-h.release
	}
	return h.r.Read(p)
}

func (h *heldReader) Seek(off int64, whence int) (int64, error) { return h.r.Seek(off, whence) }

// A publish copying its file to disk does not hold a reset off: the copy
// is made before the guard is taken, and only its publish -- a rename --
// waits for it. The reset lands while the copy is still going, and the
// publish then finds itself dropped, with nothing of it published or left
// staged.
func TestAResetDoesNotWaitOnAPublishsCopy(t *testing.T) {
	bc, wires := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	if _, err := w.acq.store.Add(worldline.Transaction{MID: testMerchant, Currency: "EUR", AmountCents: 10000, Date: settleDay}); err != nil {
		t.Fatal(err)
	}
	f := render(t, h, "", `{"currency":"EUR","date":"`+settleDay+`"}`)
	sum, err := worldline.SummarizeSettlementFile(strings.NewReader(f.Content))
	if err != nil {
		t.Fatal(err)
	}
	def, _ := w.at(tenant.Default)

	src := &heldReader{r: strings.NewReader(f.Content), reading: make(chan struct{}), release: make(chan struct{})}
	published := make(chan error, 1)
	started := w.guard.start()
	go func() {
		_, err := w.publishFile(def, started, f.Name, src, sum, true)
		published <- err
	}()
	<-src.reading

	reset := make(chan struct{})
	go func() {
		resetScope(t, w, simctl.ScopeFlow)
		close(reset)
	}()
	select {
	case <-reset:
	case <-time.After(5 * time.Second):
		close(src.release)
		t.Fatal("the reset waited on a publish copying its file")
	}
	close(src.release)
	if err := <-published; err != errReset {
		t.Fatalf("the publish the reset landed in: %v, want errReset", err)
	}
	for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic, wlsftp.DirStaging} {
		if got := names(t, w.acq.root, d); len(got) != 0 {
			t.Errorf("a dropped publish left %v in %s", got, d)
		}
	}
	if n := wires.Load(); n != 0 {
		t.Errorf("a dropped publish wired %d lump sum(s)", n)
	}
}

// countedBody counts what a client sends of a request body.
type countedBody struct {
	r    io.Reader
	sent int64
}

func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.sent += int64(n)
	return n, err
}

// A file declared over WORLDLINE_MAX_FILE_MB is refused before any of it
// is read: a caller that waits for 100 Continue, as the lab API does,
// sends none of it and reads the 413 that names the setting, not a
// connection closed under a body it was still sending.
func TestAFileOverTheLimitIsRefusedBeforeItIsSent(t *testing.T) {
	t.Setenv("WORLDLINE_MAX_FILE_MB", "1")
	bc, _ := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	srv := httptest.NewServer(ctl.Handler())
	t.Cleanup(srv.Close)

	const size = 8 << 20
	body := &countedBody{r: strings.NewReader(strings.Repeat("x", size))}
	req, err := http.NewRequest("POST", srv.URL+publishQuery("", "big.csv", "true"), body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	req.Header.Set("Expect", "100-continue")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("publish over the limit: %v, want Worldline's 413", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 413 || !strings.Contains(string(msg), "WORLDLINE_MAX_FILE_MB") {
		t.Errorf("publish over the limit: %d %s, want 413 naming WORLDLINE_MAX_FILE_MB", resp.StatusCode, msg)
	}
	if body.sent != 0 {
		t.Errorf("%d bytes of a refused file were sent, want none", body.sent)
	}
}

// Publish answers the MIDs in the order the file first names them, as a
// cut does: not sorted.
func TestPublishAnswersTheMIDsInTheFilesOrder(t *testing.T) {
	bc, _ := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	f := worldline.GenerateSettlementFile([]worldline.Transaction{
		{ID: "a", MID: "MID-A", Currency: "EUR", AmountCents: 1000, Date: settleDay},
		{ID: "z", MID: "MID-Z", Currency: "EUR", AmountCents: 2000, Date: settleDay},
	}, "EUR", settleDay, settleDay, worldline.BamboraOptions{})
	f.Batches[0], f.Batches[1] = f.Batches[1], f.Batches[0] // MID-Z first
	var content strings.Builder
	if err := f.WriteCSV(&content); err != nil {
		t.Fatal(err)
	}
	r := serve(t, h, "POST", publishQuery("", "order.csv", "false"), content.String())
	if r.Code != 201 {
		t.Fatalf("publish: %d %s", r.Code, r.Body)
	}
	var out publishedFile
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.MIDs, ",") != "MID-Z,MID-A" {
		t.Errorf("publish answered the MIDs %v, want MID-Z,MID-A as the file names them", out.MIDs)
	}
}
