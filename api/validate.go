package api

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Kinds lists the allowed kinds per source. A nil list (wiki) accepts any
// lowercase slug, because that source's kinds are not designed yet.
var Kinds = map[string][]string{
	SourceGitHub:      {"review-requested", "assigned", "pr-needs-fix"},
	SourceSlack:       {"mention", "dm-awaiting-reply", "thread-question", "saved", "reminder"},
	SourceClassroom:   {"assignment"},
	SourceGeminiNotes: {"action-item"},
	SourceEmail:       {"needs-reply"},
	SourceWiki:        nil,
}

var (
	slugRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	unixRevRe   = regexp.MustCompile(`^([0-9]{1,12})(?:\.([0-9]{1,9}))?$`)
	dateOnlyRe  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	errEmptyRev = errors.New("revision is empty")
)

// Key computes the idempotency key <source>:<account>:<kind>:<source_id>,
// with source, account and kind lowercased.
func Key(e Event) string {
	return strings.ToLower(e.Source) + ":" + strings.ToLower(e.Account) + ":" +
		strings.ToLower(e.Kind) + ":" + e.SourceID
}

// ParseRevision parses a revision: RFC 3339, or Unix seconds with an
// optional fraction of up to 9 digits.
func ParseRevision(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errEmptyRev
	}
	if m := unixRevRe.FindStringSubmatch(s); m != nil {
		sec, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("revision %q: %w", s, err)
		}
		var nsec int64
		if m[2] != "" {
			frac := m[2] + strings.Repeat("0", 9-len(m[2]))
			nsec, _ = strconv.ParseInt(frac, 10, 64)
		}
		if sec > math.MaxInt64/int64(time.Second)-1 {
			return time.Time{}, fmt.Errorf("revision %q is out of range", s)
		}
		return time.Unix(sec, nsec).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("revision %q is neither RFC 3339 nor Unix seconds", s)
	}
	if t.Year() < 1970 || t.Year() > 2200 {
		return time.Time{}, fmt.Errorf("revision %q is out of range", s)
	}
	return t.UTC(), nil
}

// ValidAccountPart reports whether s can be a source or account in a key.
func ValidAccountPart(s string) bool {
	return s != "" && utf8.RuneCountInString(s) <= 100 && !strings.ContainsAny(s, ": \t\r\n")
}

// KnownSource reports whether source is one the server accepts.
func KnownSource(source string) bool {
	_, ok := Kinds[source]
	return ok
}

// Validate checks an event's fields. It does not check sender scopes.
func (e Event) Validate() error {
	if !KnownSource(e.Source) {
		return fmt.Errorf("unknown source %q", e.Source)
	}
	if !ValidAccountPart(e.Account) {
		return errors.New("account is empty or contains ':' or spaces")
	}
	if err := validKind(e.Source, e.Kind); err != nil {
		return err
	}
	if strings.TrimSpace(e.SourceID) == "" {
		return errors.New("source_id is empty")
	}
	if utf8.RuneCountInString(e.SourceID) > MaxSourceIDRunes {
		return fmt.Errorf("source_id is longer than %d characters", MaxSourceIDRunes)
	}
	switch e.Op {
	case OpUpsert, OpResolve:
	default:
		return fmt.Errorf("op must be %q or %q", OpUpsert, OpResolve)
	}
	if _, err := ParseRevision(e.Revision); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, e.ObservedAt); err != nil {
		return errors.New("observed_at is not RFC 3339")
	}
	if e.Op == OpUpsert {
		if e.URL == "" {
			return errors.New("url is required for upsert")
		}
		if strings.TrimSpace(e.Title) == "" {
			return errors.New("title is required for upsert")
		}
	}
	if e.URL != "" {
		if utf8.RuneCountInString(e.URL) > MaxURLRunes {
			return fmt.Errorf("url is longer than %d characters", MaxURLRunes)
		}
		u, err := url.Parse(e.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("url must be an absolute http or https link")
		}
	}
	if n := utf8.RuneCountInString(e.Title); n > MaxTitleRunes {
		return fmt.Errorf("title is %d characters, at most %d", n, MaxTitleRunes)
	}
	if n := utf8.RuneCountInString(e.Summary); n > MaxSummaryRunes {
		return fmt.Errorf("summary is %d characters, at most %d", n, MaxSummaryRunes)
	}
	if utf8.RuneCountInString(e.From) > MaxFromRunes {
		return fmt.Errorf("from is longer than %d characters", MaxFromRunes)
	}
	if e.OccurredAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, e.OccurredAt); err != nil {
			return errors.New("occurred_at is not RFC 3339")
		}
	}
	if e.Due != "" && !validDue(e.Due) {
		return errors.New("due is neither YYYY-MM-DD nor RFC 3339")
	}
	if len(e.Labels) > MaxLabels {
		return fmt.Errorf("more than %d labels", MaxLabels)
	}
	for k, v := range e.Labels {
		if k == "" || utf8.RuneCountInString(k) > MaxLabelKeyRunes {
			return fmt.Errorf("label keys must be 1 to %d characters", MaxLabelKeyRunes)
		}
		if utf8.RuneCountInString(v) > MaxLabelValRunes {
			return fmt.Errorf("label %q is longer than %d characters", k, MaxLabelValRunes)
		}
	}
	return nil
}

func validKind(source, kind string) error {
	if !slugRe.MatchString(kind) {
		return fmt.Errorf("kind %q is not a lowercase slug", kind)
	}
	kinds := Kinds[source]
	if kinds == nil {
		return nil
	}
	for _, k := range kinds {
		if k == kind {
			return nil
		}
	}
	return fmt.Errorf("kind %q is not one of %s for source %s", kind, strings.Join(kinds, ", "), source)
}

func validDue(s string) bool {
	if dateOnlyRe.MatchString(s) {
		_, err := time.Parse(time.DateOnly, s)
		return err == nil
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}
