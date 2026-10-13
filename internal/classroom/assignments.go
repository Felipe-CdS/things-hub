package classroom

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Submission states that mean the student is done with the work.
const (
	StateTurnedIn = "TURNED_IN"
	StateReturned = "RETURNED"
	// StateNone marks coursework with no submission for the student.
	StateNone = "NONE"
)

// MaxDescriptionRunes bounds the description copied into an assignment.
const MaxDescriptionRunes = 1500

// OldAge is how far in the past work must lie, and stay unchanged, to be
// old: due (or, without a due date, created) and last changed before it.
const OldAge = 14 * 24 * time.Hour

// Assignment is one published coursework joined with the student's own
// submission, with the event fields the skill needs already worked out.
type Assignment struct {
	// SourceID is the event source_id: <courseId>/<courseWorkId>.
	SourceID     string  `json:"source_id"`
	CourseID     string  `json:"course_id"`
	Course       string  `json:"course"`
	Section      string  `json:"section,omitempty"`
	CourseWorkID string  `json:"coursework_id"`
	Title        string  `json:"title"`
	Description  string  `json:"description,omitempty"`
	URL          string  `json:"url"`
	WorkType     string  `json:"work_type,omitempty"`
	MaxPoints    float64 `json:"max_points,omitempty"`
	// Due is YYYY-MM-DD or RFC 3339 UTC, empty when the work has no due date.
	Due     string `json:"due,omitempty"`
	Created string `json:"created,omitempty"`
	// Revision is the later of the coursework's and the submission's
	// updateTime, RFC 3339 UTC.
	Revision string `json:"revision"`
	State    string `json:"state"`
	Late     bool   `json:"late,omitempty"`
	// Done is true when the submission is turned in or returned.
	Done bool `json:"done"`
	// Old is true when the work was due (or, without a due date, created)
	// and it, the submission and the course last changed more than OldAge
	// before the fetch.
	Old bool `json:"old,omitempty"`
}

// Assignments is the output of the assignments command.
type Assignments struct {
	Account     string       `json:"account"`
	FetchedAt   string       `json:"fetched_at"`
	Courses     int          `json:"courses"`
	Assignments []Assignment `json:"assignments"`
}

type course struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Section       string `json:"section"`
	AlternateLink string `json:"alternateLink"`
	UpdateTime    string `json:"updateTime"`
}

type date struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

type timeOfDay struct {
	Hours   int `json:"hours"`
	Minutes int `json:"minutes"`
	Seconds int `json:"seconds"`
}

type courseWork struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	AlternateLink string     `json:"alternateLink"`
	WorkType      string     `json:"workType"`
	MaxPoints     float64    `json:"maxPoints"`
	DueDate       *date      `json:"dueDate"`
	DueTime       *timeOfDay `json:"dueTime"`
	CreationTime  string     `json:"creationTime"`
	UpdateTime    string     `json:"updateTime"`
}

type submission struct {
	CourseWorkID string `json:"courseWorkId"`
	State        string `json:"state"`
	Late         bool   `json:"late"`
	UpdateTime   string `json:"updateTime"`
}

// FetchAssignments reads every active course, its published coursework and
// the student's submissions, and joins them. Any API error fails the whole
// fetch, so a returned list is always complete.
func (c *Client) FetchAssignments(ctx context.Context, account string, now time.Time) (*Assignments, error) {
	rawCourses, err := c.Courses(ctx)
	if err != nil {
		return nil, err
	}
	out := &Assignments{Account: account, FetchedAt: now.UTC().Format(time.RFC3339), Courses: len(rawCourses),
		Assignments: []Assignment{}}
	for _, rc := range rawCourses {
		var co course
		if err := json.Unmarshal(rc, &co); err != nil {
			return nil, fmt.Errorf("decode course: %w", err)
		}
		rawWork, err := c.CourseWork(ctx, co.ID)
		if err != nil {
			return nil, err
		}
		rawSubs, err := c.Submissions(ctx, co.ID)
		if err != nil {
			return nil, err
		}
		subs := map[string]submission{}
		for _, rs := range rawSubs {
			var s submission
			if err := json.Unmarshal(rs, &s); err != nil {
				return nil, fmt.Errorf("decode submission: %w", err)
			}
			subs[s.CourseWorkID] = s
		}
		for _, rw := range rawWork {
			var cw courseWork
			if err := json.Unmarshal(rw, &cw); err != nil {
				return nil, fmt.Errorf("decode coursework: %w", err)
			}
			s, ok := subs[cw.ID]
			if !ok {
				s = submission{State: StateNone}
			}
			out.Assignments = append(out.Assignments, join(co, cw, s, now))
		}
	}
	return out, nil
}

func join(co course, cw courseWork, s submission, now time.Time) Assignment {
	a := Assignment{
		SourceID:     co.ID + "/" + cw.ID,
		CourseID:     co.ID,
		Course:       co.Name,
		Section:      co.Section,
		CourseWorkID: cw.ID,
		Title:        strings.TrimSpace(cw.Title),
		Description:  truncate(strings.TrimSpace(cw.Description), MaxDescriptionRunes),
		URL:          firstLink(cw.AlternateLink, co.AlternateLink),
		WorkType:     cw.WorkType,
		MaxPoints:    cw.MaxPoints,
		Due:          due(cw.DueDate, cw.DueTime),
		Created:      rfc3339(cw.CreationTime),
		State:        s.State,
		Late:         s.Late,
		Done:         s.State == StateTurnedIn || s.State == StateReturned,
	}
	if a.State == "" {
		a.State = StateNone
	}
	a.Revision = latest(now, cw.UpdateTime, s.UpdateTime, cw.CreationTime)
	cutoff := now.Add(-OldAge)
	since := a.Due
	if since == "" {
		since = a.Created
	}
	a.Old = before(since, cutoff) && before(latest(now, a.Revision, co.UpdateTime), cutoff)
	return a
}

// before reports whether s, an RFC 3339 time or a YYYY-MM-DD date, is
// before t; an empty or unparsable s is not.
func before(s string, t time.Time) bool {
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		if v, err = time.Parse(time.DateOnly, s); err != nil {
			return false
		}
	}
	return v.Before(t)
}

// due renders a Classroom due date and time, both in UTC.
func due(d *date, t *timeOfDay) string {
	if d == nil || d.Year == 0 || d.Month == 0 || d.Day == 0 {
		return ""
	}
	if t == nil {
		return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
	}
	return time.Date(d.Year, time.Month(d.Month), d.Day, t.Hours, t.Minutes, t.Seconds, 0, time.UTC).
		Format(time.RFC3339)
}

// latest returns the latest of the given RFC 3339 times, or now when none
// parses.
func latest(now time.Time, times ...string) string {
	var best time.Time
	for _, s := range times {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.After(best) {
			best = t
		}
	}
	if best.IsZero() {
		best = now
	}
	return best.UTC().Format(time.RFC3339Nano)
}

func rfc3339(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func firstLink(links ...string) string {
	for _, l := range links {
		if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") {
			return l
		}
	}
	return "https://classroom.google.com/"
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
