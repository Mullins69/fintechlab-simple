package worldline

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Worldline delivers the same settlement content twice a day in two named
// slots. The morning file ("ER") lands in the 08:00-10:00 window and is
// the one a platform processes; the afternoon file ("AR") follows later
// the same day and is a confirmation -- same content shape, different
// slot, retained for audit rather than reprocessed.
//
// The slot codes are the real ones from the filename pattern
// BamboraFilename implements; they are not report types.
const (
	SlotMorning   = "ER"
	SlotAfternoon = "AR"
)

// SlotName maps a human slot name onto its filename code, so an operator
// can trigger "morning" without knowing that ER means morning.
func SlotName(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "morning", "er", "":
		return SlotMorning, nil
	case "afternoon", "confirmation", "ar":
		return SlotAfternoon, nil
	}
	return "", fmt.Errorf("worldline: unknown settlement slot %q (want morning|afternoon)", s)
}

// CycleConfig is everything the daily settlement cycle needs that is not
// derivable from the transactions themselves.
type CycleConfig struct {
	// Identifier is the {identifier} segment of the filename -- the
	// contract-level name Worldline assigns a receiving platform.
	Identifier string

	// MorningAt and AfternoonAt are the local clock times ("HH:MM") the
	// two slots fire at. Real Worldline delivers the morning file
	// somewhere in an 08:00-10:00 window; MorningJitter widens the fire
	// time across that window so a consumer polling for it cannot depend
	// on an exact second.
	MorningAt     string
	AfternoonAt   string
	MorningJitter time.Duration

	// SettlementDelay is how far back the cycle looks: Worldline settles
	// the *previous* day's transactions, so a cycle running on the 5th
	// cuts a file covering the 4th. One day matches the real T+1.
	SettlementDelay time.Duration

	// FixtureDir, when set, makes the cycle serve canned files from disk
	// instead of generating them (see FileSourceFixture). Use it once you
	// have a real example settlement file: the bytes are delivered
	// verbatim, so a parser is tested against the real thing rather than
	// against this lab's rendering of it.
	FixtureDir string

	// SettlementAccount is the TO_ACCOUNT the lump sum is paid into --
	// the platform's settlement account, which is what a real file names.
	SettlementAccount string

	// Source selects generated or fixture files.
	Source FileSource
}

// FileSource selects where a cycle's file content comes from.
type FileSource string

const (
	// FileSourceGenerate builds the file from the acquired transactions
	// Worldline holds. The default.
	FileSourceGenerate FileSource = "generate"
	// FileSourceFixture serves canned files from CycleConfig.FixtureDir
	// byte-for-byte, ignoring the transaction store entirely.
	FileSourceFixture FileSource = "fixture"
)

// CutFile is one settlement file the cycle produced: what it settles, not
// its bytes. It is what a cycle run keeps in its history (a file of
// millions of transactions is hundreds of megabytes the history has no use
// for); the bytes are a Cutting's to write, or a RenderedFile's.
type CutFile struct {
	Filename string `json:"filename"`
	Slot     string `json:"slot"`
	// MIDs lists every submerchant with rows in this file, in the order
	// the file first names them. A settlement file is per currency, not
	// per merchant -- see GenerateSettlementFile for why the filename
	// carries no merchant.
	MIDs     []string `json:"mids"`
	Currency string   `json:"currency"`
	// AmountCents is the file's own SETTLEMENT_AMOUNT -- the figure the
	// lump sum landing in the platform's safeguarding account must match.
	AmountCents int64  `json:"amount_cents"`
	Items       int    `json:"items"`
	FromDate    string `json:"from_date"`
	ToDate      string `json:"to_date"`
}

// Cutting is a file the cycle cuts, not yet written: what it settles, and
// Write, which renders it to w as it goes. A file of millions of
// transactions goes to w row by row rather than being held whole first.
type Cutting struct {
	CutFile
	Write func(w io.Writer) error
}

// RenderedFile is a cut file with its bytes, as Cut holds it.
type RenderedFile struct {
	CutFile
	Bytes []byte
}

// Cut builds every settlement file for the given slot covering
// [fromDate, toDate], one per currency, and holds each one's bytes. It
// does not write anything, which keeps it testable without a filesystem;
// the cycle itself writes each file as it renders it (CutStreams).
func Cut(store *Store, cfg CycleConfig, slot, fromDate, toDate string, at time.Time) ([]RenderedFile, error) {
	cuts, err := CutStreams(store, cfg, slot, fromDate, toDate, at)
	if err != nil {
		return nil, err
	}
	out := make([]RenderedFile, 0, len(cuts))
	for _, c := range cuts {
		var buf bytes.Buffer
		if err := c.Write(&buf); err != nil {
			return nil, fmt.Errorf("worldline: render %s: %w", c.Filename, err)
		}
		out = append(out, RenderedFile{CutFile: c.CutFile, Bytes: buf.Bytes()})
	}
	return out, nil
}

// CutStreams is Cut with each file left to be written: the files of the
// slot covering [fromDate, toDate], one per currency, each with what it
// settles and Write to render it.
func CutStreams(store *Store, cfg CycleConfig, slot, fromDate, toDate string, at time.Time) ([]Cutting, error) {
	if cfg.Source == FileSourceFixture {
		return cutFromFixtures(cfg, slot, fromDate, toDate, at)
	}
	// One settlement file per currency, covering every submerchant -- the
	// shape the real filename pattern implies (it carries no merchant).
	// The platform splits it per MID on the way in.
	var out []Cutting
	txns := store.All()
	for _, ccy := range store.CurrenciesInRange("", fromDate, toDate) {
		if c, ok := cutGenerated(txns, cfg, slot, ccy, fromDate, toDate, at); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// CutCurrency is CutStreams for one currency: what the file settles, and
// write, which writes it to w as Cut would have rendered it, byte for
// byte. ok is false when nothing in ccy settles in [fromDate, toDate].
//
// From fixtures, the file is the first of the slot's that is a settlement
// file in ccy, read row by row to learn its currency, MIDs and total.
func CutCurrency(store *Store, cfg CycleConfig, slot, ccy, fromDate, toDate string, at time.Time) (f CutFile, write func(w io.Writer) error, ok bool, err error) {
	ccy = strings.ToUpper(ccy)
	if cfg.Source != FileSourceFixture {
		c, ok := cutGenerated(store.All(), cfg, slot, ccy, fromDate, toDate, at)
		return c.CutFile, c.Write, ok, nil
	}
	cuts, err := cutFromFixtures(cfg, slot, fromDate, toDate, at)
	if err != nil {
		return CutFile{}, nil, false, err
	}
	for _, c := range cuts {
		sum, err := summarizeCutting(c)
		if err != nil || strings.ToUpper(sum.Meta.SettlementCurrency) != ccy {
			continue
		}
		c.Currency, c.MIDs = ccy, sum.MIDs
		c.AmountCents, c.Items = sum.Meta.SettlementAmount, sum.Meta.NumberOfItems
		return c.CutFile, c.Write, true, nil
	}
	return CutFile{}, nil, false, nil
}

// summarizeCutting reads what c writes as a settlement file, row by row.
func summarizeCutting(c Cutting) (SettlementSummary, error) {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(c.Write(pw)) }()
	defer pr.Close()
	return SummarizeSettlementFile(pr)
}

// cutGenerated is the file of one currency the cycle generates from txns;
// ok is false when it would have no items.
func cutGenerated(txns []Transaction, cfg CycleConfig, slot, ccy, fromDate, toDate string, at time.Time) (Cutting, bool) {
	f := GenerateSettlementFile(txns, ccy, fromDate, toDate, BamboraOptions{
		ValueDate: toDate,
		ToAccount: cfg.SettlementAccount,
	})
	if f.Meta.NumberOfItems == 0 {
		return Cutting{}, false
	}
	mids := make([]string, 0, len(f.Batches))
	for _, b := range f.Batches {
		if len(b.Transactions) > 0 {
			mids = append(mids, b.Transactions[0].AdditionalRef2)
		}
	}
	return Cutting{
		CutFile: CutFile{
			Filename:    BamboraFilename(cfg.Identifier, at, slot, ccy),
			Slot:        slot,
			MIDs:        mids,
			Currency:    ccy,
			AmountCents: f.Meta.SettlementAmount,
			Items:       f.Meta.NumberOfItems,
			FromDate:    fromDate,
			ToDate:      toDate,
		},
		Write: f.WriteCSV,
	}, true
}

// cutFromFixtures serves the canned files in cfg.FixtureDir whose names
// carry this slot's code, verbatim. Filenames are kept exactly as they sit
// on disk -- a real example file's own name is part of what a consumer's
// filename validator is being tested against, so rewriting it would defeat
// the point of supplying one. Each is read from disk as it is written, so
// a real file of any size is served without being held.
//
// MID, currency and amount are left zero: the bytes are somebody else's
// and this package does not re-parse them to guess. A consumer reading the
// file learns those from the file, which is the honest arrangement.
func cutFromFixtures(cfg CycleConfig, slot, fromDate, toDate string, _ time.Time) ([]Cutting, error) {
	if cfg.FixtureDir == "" {
		return nil, fmt.Errorf("worldline: file source is %q but no fixture directory is configured", FileSourceFixture)
	}
	entries, err := os.ReadDir(cfg.FixtureDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worldline: read fixture dir %s: %w", cfg.FixtureDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.Contains(e.Name(), "_"+slot+"_") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var out []Cutting
	for _, name := range names {
		path := filepath.Join(cfg.FixtureDir, name)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("worldline: read fixture %s: %w", name, err)
		}
		out = append(out, Cutting{
			CutFile: CutFile{Filename: name, Slot: slot, FromDate: fromDate, ToDate: toDate},
			Write: func(w io.Writer) error {
				f, err := os.Open(path)
				if err != nil {
					return fmt.Errorf("worldline: read fixture %s: %w", name, err)
				}
				defer f.Close()
				_, err = io.Copy(w, f)
				return err
			},
		})
	}
	return out, nil
}

// CoverageFor returns the [fromDate, toDate] a cycle running at `at`
// settles: a single day, cfg.SettlementDelay in the past (T+1 by default).
func (cfg CycleConfig) CoverageFor(at time.Time) (fromDate, toDate string) {
	delay := cfg.SettlementDelay
	if delay == 0 {
		delay = 24 * time.Hour
	}
	d := at.Add(-delay).UTC().Format("2006-01-02")
	return d, d
}

// NextFire returns the next time slot fires after `now`, and the slot code
// for it. Both slots are checked so a caller can sleep until whichever
// comes first rather than polling.
func (cfg CycleConfig) NextFire(now time.Time) (time.Time, string) {
	morning := nextClock(now, cfg.MorningAt, "08:30")
	afternoon := nextClock(now, cfg.AfternoonAt, "15:30")
	if morning.Before(afternoon) {
		return morning.Add(jitterFor(morning, cfg.MorningJitter)), SlotMorning
	}
	return afternoon, SlotAfternoon
}

// nextClock returns the next occurrence of the "HH:MM" clock time after
// now, falling back to def when clock is unset or unparseable -- a
// malformed schedule must not stop the simulator booting, it must run on
// the documented default and say so through the caller's log.
func nextClock(now time.Time, clock, def string) time.Time {
	hh, mm, err := parseClock(clock)
	if err != nil {
		hh, mm, _ = parseClock(def)
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func parseClock(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, fmt.Errorf("worldline: %q is not HH:MM: %w", s, err)
	}
	return t.Hour(), t.Minute(), nil
}

// jitterFor spreads the morning delivery deterministically across the
// window rather than randomly, so two runs of the same scenario on the
// same day fire at the same second and a test can be repeatable while the
// consumer still cannot assume an exact time.
func jitterFor(at time.Time, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	seed := bamboraSeed("morning-jitter", at.UTC().Format("2006-01-02"))
	return time.Duration(seed % uint64(window))
}
