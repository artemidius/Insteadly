package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/artemidius/insteadly/internal/core/domain"
)

// The IVR parser is deliberately small, deterministic, and core-owned. It recognizes a
// single menu digit only when the final transcript contains an explicit menu imperative
// adjacent to the digit and a purpose, and only presses a unique route that matches the
// appointment/reception target. Numbers in dates, phone numbers, hours, and ordinary
// speech are not menu options.
var (
	menuImperatives = map[string]bool{
		"druecken": true, "drueck": true, "waehlen": true, "waehl": true,
		"press": true, "dial": true, "taste": true, "tippen": true, "geben": true,
	}
	menuTargets = map[string]bool{
		"termin": true, "termine": true, "terminvereinbarung": true, "terminvergabe": true, "anmeldung": true,
		"rezeption": true, "ordination": true, "appointment": true, "appointments": true,
		"reception": true, "booking": true,
	}
	menuExclusions = map[string]bool{
		"notfall": true, "emergency": true, "rettung": true, "ambulance": true,
		"rezept": true, "rechnung": true, "labor": true, "befund": true,
		"billing": true, "records": true, "results": true,
	}
	menuNumberWords = map[string]string{
		"null": "0", "zero": "0",
		"eins": "1", "one": "1",
		"zwei": "2", "two": "2",
		"drei": "3", "three": "3",
		"vier": "4", "four": "4",
		"fuenf": "5", "five": "5",
		"sechs": "6", "six": "6",
		"sieben": "7", "seven": "7",
		"acht": "8", "eight": "8",
		"neun": "9", "nine": "9",
		"sternchen": "*", "star": "*", "raute": "#", "hash": "#",
	}
)

// menuOption is one parsed digit option with its purpose classification.
type menuOption struct {
	Digit  string
	Target bool
}

// menuParse is the deterministic parse of one final transcript.
type menuParse struct {
	IsMenu    bool
	Options   []menuOption
	Targets   []string
	Ambiguous bool
	NoRoute   bool
	Digest    string
}

// parseMenu recognizes bounded German/English menu syntax on a final transcript only.
func parseMenu(text string) menuParse {
	norm := normalizeMenuText(text)
	p := menuParse{}
	if norm == "" {
		return p
	}
	p.Digest = menuDigest(norm)
	p.NoRoute = strings.Contains(norm, "keine auswahl") ||
		strings.Contains(norm, "keine option") ||
		strings.Contains(norm, "keine moeglichkeit") ||
		strings.Contains(norm, "keine route") ||
		strings.Contains(norm, "no option") ||
		strings.Contains(norm, "no route")

	for _, clause := range splitMenuClauses(strings.Fields(norm)) {
		if !containsAny(clause, menuImperatives) {
			continue
		}
		for _, opt := range clauseOptions(clause) {
			p.Options = append(p.Options, opt)
			p.IsMenu = true
		}
	}
	if !p.IsMenu {
		return p
	}
	seen := map[string]bool{}
	for _, opt := range p.Options {
		if opt.Target && !seen[opt.Digit] {
			seen[opt.Digit] = true
			p.Targets = append(p.Targets, opt.Digit)
		}
	}
	p.Ambiguous = len(p.Targets) > 1
	return p
}

// splitMenuClauses breaks a normalized menu on hard punctuation. Coordinating words such as
// "oder"/"or" stay in the clause so an imperative may be shared, while each digit's purpose
// is still scoped to its own nearest-digit segment by clauseOptions.
func splitMenuClauses(tokens []string) [][]string {
	var clauses [][]string
	var cur []string
	for _, tok := range tokens {
		if tok == "|" {
			if len(cur) > 0 {
				clauses = append(clauses, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, tok)
	}
	if len(cur) > 0 {
		clauses = append(clauses, cur)
	}
	return clauses
}

// clauseOptions partitions a clause's tokens by nearest digit so each option's purpose is
// local: in "press 1 for office hours or 2 for appointments" the words around "1" cannot
// supply the purpose for "2". The clause-level imperative check remains shared, which is
// how coordinated menus ("drücken Sie 1 für Termine oder 2 für …") are expressed.
func clauseOptions(clause []string) []menuOption {
	var digitIdx []int
	for i, tok := range clause {
		if isMenuDigit(tok) {
			digitIdx = append(digitIdx, i)
		}
	}
	if len(digitIdx) == 0 {
		return nil
	}
	opts := make([]menuOption, 0, len(digitIdx))
	for k, idx := range digitIdx {
		start := 0
		if k > 0 {
			start = (digitIdx[k-1]+idx)/2 + 1
		}
		end := len(clause) - 1
		if k < len(digitIdx)-1 {
			end = (idx + digitIdx[k+1]) / 2
		}
		if start < 0 {
			start = 0
		}
		if end >= len(clause) {
			end = len(clause) - 1
		}
		if start > idx || end < idx {
			continue
		}
		seg := clause[start : end+1]
		digit := clause[idx]
		if mapped, ok := menuNumberWords[digit]; ok && len(mapped) == 1 {
			digit = mapped
		}
		opt := menuOption{Digit: digit}
		if containsAny(seg, menuTargets) && !containsAny(seg, menuExclusions) {
			opt.Target = true
		}
		opts = append(opts, opt)
	}
	return opts
}

func normalizeMenuText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range strings.ToLower(text) {
		switch r {
		case 'ä':
			b.WriteString("ae")
		case 'ö':
			b.WriteString("oe")
		case 'ü':
			b.WriteString("ue")
		case 'ß':
			b.WriteString("ss")
		case '*', '#':
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		case '.', ',', ';', ':', '!', '?', '|', '\n':
			b.WriteString(" | ")
		default:
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		}
	}
	fields := strings.Fields(b.String())
	for i, f := range fields {
		if mapped, ok := menuNumberWords[f]; ok {
			fields[i] = mapped
		}
	}
	return strings.Join(fields, " ")
}

func menuDigest(normalized string) string {
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:8])
}

func isMenuDigit(tok string) bool {
	if mapped, ok := menuNumberWords[tok]; ok && len(mapped) == 1 {
		return true
	}
	if len(tok) != 1 {
		return false
	}
	c := tok[0]
	return (c >= '0' && c <= '9') || c == '*' || c == '#'
}

func containsAny(tokens []string, set map[string]bool) bool {
	for _, t := range tokens {
		if set[t] {
			return true
		}
	}
	return false
}

// ivrState is per-call IVR navigation state. It stores only digests/digits/steps, never
// menu transcript text.
type ivrState struct {
	active         bool
	steps          int
	seen           map[string]int
	lastSentDigest string
	sentAt         time.Time
	waitUntil      time.Time
}

// handleMenuTranscript applies the bounded navigation policy to one final transcript. It
// reports handled=false for ordinary human speech, handled=true when the transcript was a
// menu (never sent to dialogue), and a non-nil outcome for an honest ivr_dead_end.
func (a *Agent) handleMenuTranscript(ctx context.Context, call AudioSession, st *callState, text string) (bool, *domain.Outcome, error) {
	p := parseMenu(text)
	if !p.IsMenu {
		// Human speech exits IVR mode and must also cancel the post-digit timeout: the
		// caller is now in ordinary dialogue, so a stale menu timer cannot force a dead end.
		st.ivr.active = false
		st.ivr.waitUntil = time.Time{}
		return false, nil, nil
	}
	st.ivr.active = true
	if st.ivr.seen == nil {
		st.ivr.seen = make(map[string]int)
	}
	st.ivr.seen[p.Digest]++
	// A new final menu is activity after a sent digit: give the far end another full
	// window, while repeated identical menus still dead-end below.
	if st.ivr.steps > 0 {
		st.ivr.waitUntil = time.Now().Add(a.cfg.IVRWaitTimeout)
	}

	// A repeated menu after any accepted digit is a loop, not a fresh choice.
	if st.ivr.seen[p.Digest] >= 2 && st.ivr.steps > 0 {
		out := ivrDeadEndOutcome()
		return true, &out, nil
	}
	if p.NoRoute || len(p.Targets) != 1 {
		if p.NoRoute && st.ivr.seen[p.Digest] >= 2 {
			out := ivrDeadEndOutcome()
			return true, &out, nil
		}
		if len(p.Targets) != 1 && st.ivr.seen[p.Digest] >= 2 {
			out := ivrDeadEndOutcome()
			return true, &out, nil
		}
		// First presentation of an unusable/ambiguous menu: wait for a clearer repeat.
		return true, nil, nil
	}

	digit := p.Targets[0]
	if p.Digest == st.ivr.lastSentDigest {
		out := ivrDeadEndOutcome()
		return true, &out, nil
	}
	if st.ivr.steps >= a.cfg.MaxIVRSteps {
		out := ivrDeadEndOutcome()
		return true, &out, nil
	}
	if err := call.SendDTMF(ctx, digit); err != nil {
		// A send failure means no route was navigated: operational error, not a dead end.
		return true, nil, fmt.Errorf("send ivr digit: %w", err)
	}
	st.ivr.steps++
	st.ivr.lastSentDigest = p.Digest
	st.ivr.sentAt = a.cfg.Now()
	// The 15 s bound is wall-clock operational timing, not prompt/tool time. Keep the
	// injected clock only for recorded evidence.
	st.ivr.waitUntil = time.Now().Add(a.cfg.IVRWaitTimeout)
	return true, nil, nil
}
