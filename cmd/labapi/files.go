package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/atomicfile"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// The environment's platform card (docs/lab-api/plan.md, phase 6): its
// settlement files, kept by the lab per environment on its data volume;
// publishing them at Worldline, in the environment's SFTP folder, as the
// morning's file; and the platform's own poll, which the card can call
// instead of waiting for the next window.
//
//	<data>/files/<env>/generated/  cut from the environment's trading
//	<data>/files/<env>/uploaded/   brought from anywhere

const (
	// sourceGenerated files the platform's seeder made from its seeded
	// merchants (POST /v1/settlement-files/generate).
	sourceGenerated = "generated"
	// sourceCut files Worldline cut from the lab's own trading
	// (POST /v1/settlement-files/cut).
	sourceCut      = "cut"
	sourceUploaded = "uploaded"
)

// sources are the folders a file can be in, in listing order.
var sources = []string{sourceGenerated, sourceCut, sourceUploaded}

var fileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// settlementFile is one file as the card lists it.
type settlementFile struct {
	Name     string    `json:"name"`
	Source   string    `json:"source"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
	Currency string    `json:"currency,omitempty"`
	MIDs     []string  `json:"mids,omitempty"`
	Total    string    `json:"total,omitempty"`
	Items    int       `json:"items,omitempty"`
	// Problem is why Worldline's parser refuses it: a file with one cannot
	// be published.
	Problem string `json:"problem,omitempty"`
}

// filesDir is the call's environment's folder for source.
func (a *app) filesDir(ctx context.Context, source string) string {
	return filepath.Join(a.dataDir, "files", scopeOf(ctx), source)
}

// tmpDir is where an upload or a cut is written before it is renamed into
// its folder (internal/atomicfile): on the lab's volume beside every
// environment's files, since a rename is only whole within one
// filesystem, and in none of the folders a listing reads, so a file still
// arriving is never listed. No environment id starts with a dot. What a
// lab API that stopped mid-write left here goes at start-up (sweepTmp).
func (a *app) tmpDir() string {
	return filepath.Join(a.dataDir, "files", ".tmp")
}

// sweepTmp removes what a stopped upload or cut left in tmpDir. Call it at
// start-up, before anything can be writing there.
func (a *app) sweepTmp() {
	if n, err := atomicfile.Sweep(a.tmpDir()); err != nil {
		log.Printf("lab-api: sweep %s: %v", a.tmpDir(), err)
	} else if n > 0 {
		log.Printf("lab-api: removed %d file(s) a stopped upload or cut left in %s", n, a.tmpDir())
	}
}

// removeEnvFiles takes an environment's settlement files off the lab's
// volume, every source at once, and says how many there were. A deleted
// environment's files go with it, and an environment created under an id
// one had before starts without them: they pay MIDs of a platform that is
// no longer there. The default environment is never one of these.
func (a *app) removeEnvFiles(id string) (int, error) {
	if tenant.Valid(id) != nil {
		return 0, nil
	}
	dir := filepath.Join(a.dataDir, "files", id)
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n, os.RemoveAll(dir)
}

// findFile is where name is, in either folder.
func (a *app) findFile(ctx context.Context, name string) (string, string, bool) {
	for _, src := range sources {
		p := filepath.Join(a.filesDir(ctx, src), name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, src, true
		}
	}
	return "", "", false
}

func validName(w http.ResponseWriter, name string) bool {
	if !fileName.MatchString(name) || strings.Contains(name, "..") {
		httputilx.Error(w, 400, "a file name is letters, digits, dot, dash and underscore, at most 200, not starting with a dot")
		return false
	}
	return true
}

// describe reads a file with Worldline's own parser.
func describe(path, source string, st os.FileInfo) settlementFile {
	f := settlementFile{Name: st.Name(), Source: source, Bytes: st.Size(), Modified: st.ModTime().UTC()}
	r, err := os.Open(path)
	if err != nil {
		f.Problem = err.Error()
		return f
	}
	defer r.Close()
	// Worldline's own check, read row by row: a file may be millions of
	// transactions, and only its summary is kept.
	sum, err := worldline.SummarizeSettlementFile(r)
	if err != nil {
		f.Problem = "not a Worldline settlement file: " + err.Error()
		return f
	}
	f.Currency = sum.Meta.SettlementCurrency
	f.Total = money.Format(sum.Meta.SettlementAmount)
	f.Items = sum.Meta.NumberOfItems
	f.MIDs = sum.MIDs
	if f.Currency == "" {
		// The parser is lenient: text that is not a settlement file parses
		// to nothing, and nothing names no currency.
		f.Problem = "not a Worldline settlement file: it names no settlement currency"
		f.Total, f.Items, f.MIDs = "", 0, nil
	}
	return f
}

func (a *app) listSettlementFiles(w http.ResponseWriter, r *http.Request) {
	out := []settlementFile{}
	for _, src := range sources {
		dir := a.filesDir(r.Context(), src)
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			httputilx.Error(w, 500, err.Error())
			return
		}
		for _, e := range entries {
			// A name starting with a dot is never a settlement file of
			// the lab's (validName refuses one): an older lab's
			// half-written upload, say.
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			st, err := e.Info()
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			out = append(out, describe(filepath.Join(dir, e.Name()), src, st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	httputilx.WriteJSON(w, 200, map[string]any{"env": scopeOf(r.Context()), "files": out})
}

func (a *app) uploadSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	if _, src, ok := a.findFile(r.Context(), name); ok && src != sourceUploaded {
		httputilx.Error(w, 409, name+" is a "+src+" file of this environment's; upload it under another name")
		return
	}
	dir := a.filesDir(r.Context(), sourceUploaded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	tmp, err := atomicfile.Create(a.tmpDir())
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	defer tmp.Discard()
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, uploadLimit()))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		httputilx.Error(w, 413, fmt.Sprintf("larger than CONSOLE_UPLOAD_MAX_MB (%d MB)", uploadLimit()>>20))
		return
	case err != nil:
		httputilx.Error(w, 400, err.Error())
		return
	case n == 0:
		httputilx.Error(w, 400, "the file is empty")
		return
	}
	path := filepath.Join(dir, name)
	if err := tmp.Commit(path); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	st, _ := os.Stat(path)
	f := describe(path, sourceUploaded, st)
	note := "kept as " + name
	if f.Problem != "" {
		note += "; " + f.Problem
	}
	httputilx.WriteJSON(w, 201, map[string]any{"file": f, "note": note})
}

// cutSettlementFile keeps the file Worldline would cut from the
// environment's trading in the lab: for a platform with no seeder of its
// own, or a file whose MIDs the platform need not know.
func (a *app) cutSettlementFile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Currency string `json:"currency"`
		Date     string `json:"date"`
		Slot     string `json:"slot"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"currency": "EUR", "date": "YYYY-MM-DD", "slot": "ER"}: `+err.Error())
		return
	}
	if b.Currency == "" {
		httputilx.Error(w, 400, "currency is required")
		return
	}
	// The file comes back as itself, streamed, and goes to disk as it
	// comes: a cut of millions of transactions is never held here.
	req, _ := json.Marshal(b)
	ctx, cancel := context.WithTimeout(r.Context(), fileCallTimeout)
	defer cancel()
	resp, err := a.controlStream(ctx, http.MethodPost, "worldline", "/sim/settlement-files/render",
		"application/json", bytes.NewReader(req), int64(len(req)), "text/csv")
	if err != nil {
		httputilx.Error(w, 502, "worldline: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		httputilx.Error(w, resp.StatusCode, controlError(resp))
		return
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/csv" {
		httputilx.Error(w, 502, "worldline answered "+strconv.Quote(mt)+", not the file: is its image older than the lab API's?")
		return
	}
	name := resp.Header.Get("Lab-File-Name")
	if !fileName.MatchString(name) {
		httputilx.Error(w, 502, "worldline named the file "+strconv.Quote(name))
		return
	}
	dir := a.filesDir(r.Context(), sourceCut)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	tmp, err := atomicfile.Create(a.tmpDir())
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	defer tmp.Discard()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		httputilx.Error(w, 502, "worldline: the file was cut short: "+err.Error())
		return
	}
	path := filepath.Join(dir, name)
	if err := tmp.Commit(path); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	st, _ := os.Stat(path)
	httputilx.WriteJSON(w, 201, map[string]any{"file": describe(path, sourceCut, st)})
}

func (a *app) previewSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	path, _, ok := a.findFile(r.Context(), name)
	if !ok {
		httputilx.Error(w, 404, "no file "+name+" in this environment")
		return
	}
	lines := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil {
		lines = min(max(v, 1), 1000)
	}
	f, err := os.Open(path)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	out, long := []string{}, false
	for len(out) < lines && sc.Scan() {
		line := sc.Text()
		if len(line) > 2000 {
			line, long = line[:2000], true
		}
		out = append(out, line)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"name": name, "lines": out, "truncated": sc.Scan(), "long_lines": long, "bytes": st.Size()})
}

// downloadSettlementFile hands one of the environment's files over whole,
// as a download named as it is listed.
func (a *app) downloadSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	path, _, ok := a.findFile(r.Context(), name)
	if !ok {
		httputilx.Error(w, 404, "no file "+name+" in this environment")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (a *app) deleteSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	path, _, ok := a.findFile(r.Context(), name)
	if !ok {
		httputilx.Error(w, 404, "no file "+name+" in this environment")
		return
	}
	if err := os.Remove(path); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// publishSettlementFiles has Worldline publish the files the card ticked
// in the environment's SFTP folder: at most one per currency, since a
// platform settles two currencies at once but never two files of one.
func (a *app) publishSettlementFiles(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Files []string `json:"files"`
		Wire  *bool    `json:"wire"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"files": ["…"], "wire": true}: `+err.Error())
		return
	}
	if len(b.Files) == 0 {
		httputilx.Error(w, 400, "files names at least one file")
		return
	}
	wire := b.Wire == nil || *b.Wire
	type pick struct {
		name, path string
		file       settlementFile
	}
	var picks []pick
	byCurrency := map[string]string{}
	for _, name := range b.Files {
		if !validName(w, name) {
			return
		}
		path, src, ok := a.findFile(r.Context(), name)
		if !ok {
			httputilx.Error(w, 404, "no file "+name+" in this environment")
			return
		}
		st, _ := os.Stat(path)
		f := describe(path, src, st)
		if f.Problem != "" {
			httputilx.Error(w, 400, name+": "+f.Problem)
			return
		}
		if other, dup := byCurrency[f.Currency]; dup {
			httputilx.Error(w, 409, fmt.Sprintf("%s and %s are both %s: a platform settles one file per currency at a time", other, name, f.Currency))
			return
		}
		byCurrency[f.Currency] = name
		picks = append(picks, pick{name, path, f})
	}
	results := make([]map[string]any, 0, len(picks))
	ok := 0
	for _, p := range picks {
		res, err := a.publishOne(r.Context(), p.name, p.path, wire)
		if err != nil {
			results = append(results, map[string]any{"name": p.name, "error": err.Error()})
			continue
		}
		ok++
		results = append(results, res)
	}
	status := 200
	if ok == 0 {
		status = 502
	}
	httputilx.WriteJSON(w, status, map[string]any{
		"env": scopeOf(r.Context()), "published": ok, "results": results,
		"note": fmt.Sprintf("%d of %d published in the environment's SFTP folder; the platform's next poll picks them up, or press Poll now", ok, len(picks)),
	})
}

// publishOne has Worldline publish the file at path, streamed from disk:
// a file of hundreds of megabytes is never held here, and its call has
// fileCallTimeout, not a JSON call's seconds.
func (a *app) publishOne(ctx context.Context, name, path string, wire bool) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fileCallTimeout)
	defer cancel()
	q := url.Values{"name": {name}, "wire": {strconv.FormatBool(wire)}}
	resp, err := a.controlStream(ctx, http.MethodPost, "worldline", "/sim/settlement-files/publish?"+q.Encode(),
		"text/csv", f, st.Size(), "")
	if err != nil {
		return nil, fmt.Errorf("worldline: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("worldline: HTTP %d: %s", resp.StatusCode, controlError(resp))
	}
	// A 2xx is a published file, and perhaps a wired lump sum, whatever
	// comes after it. An answer that cannot be read -- it lists every MID,
	// and a file can name more than any bound -- must not read as a failure
	// that a retry would publish, and wire, a second time.
	out := map[string]any{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxControlAnswer)).Decode(&out); err != nil {
		out = map[string]any{
			"name":    name,
			"warning": "published, but Worldline's answer could not be read (" + err.Error() + "): list the SFTP folder for what it holds",
		}
	}
	// Worldline names the file it published (a Worldline-named file gets
	// the moment's timestamp); file is the lab's copy.
	out["file"] = name
	return out, nil
}

// pollClient is the one for a platform's poll, which downloads files
// before it answers.
var pollClient = &http.Client{Timeout: 90 * time.Second}

// platformPoll calls the poll the environment's config names: the
// platform's own Worldline poll, run now instead of at its next window.
func (a *app) platformPoll(w http.ResponseWriter, r *http.Request) {
	var b struct {
		FileType string `json:"fileType"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"fileType": "ER"|"AR"}: `+err.Error())
		return
	}
	switch b.FileType = strings.ToUpper(b.FileType); b.FileType {
	case "ER", "AR":
	default:
		httputilx.Error(w, 400, `fileType is "ER" (the morning file) or "AR" (the afternoon's)`)
		return
	}
	cfg, err := a.envConfig(r.Context())
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	if cfg.PollURL == "" {
		httputilx.Error(w, 409, "this environment's config names no poll_url: PATCH /v1/environments/"+scopeOf(r.Context())+` {"config": {"poll_url": "…", "poll_token": "…"}}`)
		return
	}
	body, _ := json.Marshal(b)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.PollURL, bytes.NewReader(body))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.PollToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.PollToken)
	}
	resp, err := pollClient.Do(req)
	if err != nil {
		httputilx.Error(w, 502, "the platform's poll at "+cfg.PollURL+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// envConfig is the call's environment's config, the default's included.
func (a *app) envConfig(ctx context.Context) (labdb.Config, error) {
	if a.db == nil {
		return labdb.Config{}, nil
	}
	env := scopeOf(ctx)
	if env == tenant.Default {
		return a.db.DefaultConfig(ctx)
	}
	e, err := a.db.Get(ctx, env)
	return e.Config, err
}

// controlJSON makes one JSON call to a lab service's control port in the
// call's environment (ctx names it) and decodes a 2xx answer into out; any
// other answer comes back as its status and its error message.
func (a *app) controlJSON(ctx context.Context, method, svc, path string, in, out any) (int, string, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return 0, "", err
		}
	}
	return a.controlCall(ctx, method, svc, path, "application/json", body, out)
}

// fileCallTimeout bounds a call that carries a settlement file to or from
// a control port: hundreds of megabytes, rendered or checked row by row on
// the other side, take minutes, not a JSON call's seconds.
const fileCallTimeout = 15 * time.Minute

// maxControlAnswer bounds the JSON answer of a control port the lab API
// reads: room for a listing of thousands, not a sim's runaway answer.
const maxControlAnswer = 64 << 20

// expectContinueOver is the body size from which a call to a control port
// asks first whether the body is wanted (Expect: 100-continue): a service
// that refuses it -- a file over Worldline's WORLDLINE_MAX_FILE_MB --
// answers before any of it is sent, rather than closing the connection
// under it, which the caller would read as a broken pipe instead of the
// refusal.
const expectContinueOver = 1 << 20

// controlRequest is one call to a lab service's control port in the call's
// environment (ctx names it), with body, size bytes long, as ctype.
func (a *app) controlRequest(ctx context.Context, method, svc, path, ctype string, body io.Reader, size int64) (*http.Request, error) {
	base := a.controlURL(svc)
	if base == "" {
		return nil, fmt.Errorf("%s has no control port", svc)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
	}
	return req, nil
}

// controlStream makes one call to a lab service's control port with body
// streamed as it is read, size bytes long, and returns the answer for the
// caller to read and close. It is a.client without its whole-call timeout:
// ctx bounds the call instead, so a file's call can take as long as it
// needs.
func (a *app) controlStream(ctx context.Context, method, svc, path, ctype string, body io.Reader, size int64, accept string) (*http.Response, error) {
	req, err := a.controlRequest(ctx, method, svc, path, ctype, body, size)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if size > expectContinueOver {
		req.Header.Set("Expect", "100-continue")
	}
	client := &http.Client{Transport: a.client.Transport, CheckRedirect: a.client.CheckRedirect}
	return client.Do(req)
}

// controlCall makes one call to a lab service's control port with body as
// ctype, and decodes a 2xx answer, bounded, into out; any other answer
// comes back as its status and its error message.
func (a *app) controlCall(ctx context.Context, method, svc, path, ctype string, body []byte, out any) (int, string, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := a.controlRequest(ctx, method, svc, path, ctype, rd, int64(len(body)))
	if err != nil {
		return 0, "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return resp.StatusCode, controlError(resp), nil
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxControlAnswer)).Decode(out); err != nil {
			return resp.StatusCode, "", err
		}
	}
	return resp.StatusCode, "", nil
}

// controlError is a control port's refusal: its {"error"} when it has one,
// else its body, bounded.
func controlError(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(raw))
}
