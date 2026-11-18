package sessions

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"
)

// A page's file choosers, as the person in control sees them (PROJ-181).
// The browser container's portal holds each one (PROJ-152) and answers it
// only with files from the profile's uploads; the agent's side, with its
// accept lists and notices, is internal/api's chooser.go.

// Chooser is a page's file chooser waiting in a browser.
type Chooser struct {
	ID       string
	Multiple bool
	// Left is how long until the browser cancels it unanswered.
	Left time.Duration
}

var (
	// ErrChooserClosed: no open chooser has that id (answered, cancelled,
	// closed by the page, or timed out).
	ErrChooserClosed = errors.New("that file chooser closed")
	// ErrNoPortal: the browser has no portal (an image from before it).
	ErrNoPortal = errors.New("this browser has no file chooser portal")
)

var chooserIDRE = regexp.MustCompile(`^c[0-9]{1,9}$`)

// Choosers lists the file choosers open in the profile's browser, oldest
// first.
func (m *Manager) Choosers(ctx context.Context, profile string) ([]Chooser, error) {
	var in struct {
		Choosers []struct {
			ID       string `json:"id"`
			Multiple bool   `json:"multiple"`
			LeftMS   int64  `json:"left_ms"`
		} `json:"choosers"`
	}
	if err := m.control(ctx, profile, http.MethodGet, "/chooser", nil, &in); err != nil {
		return nil, chooserError(err)
	}
	out := make([]Chooser, 0, len(in.Choosers))
	for _, c := range in.Choosers {
		if chooserIDRE.MatchString(c.ID) {
			out = append(out, Chooser{ID: c.ID, Multiple: c.Multiple, Left: time.Duration(max(0, c.LeftMS)) * time.Millisecond})
		}
	}
	return out, nil
}

// AnswerChooser answers chooser id in the profile's browser with the
// uploads named (bare names, which the caller has checked are uploads,
// and the browser checks again), or cancels it when there are none.
func (m *Manager) AnswerChooser(ctx context.Context, profile, id string, uploads []string) error {
	if !chooserIDRE.MatchString(id) {
		return ErrChooserClosed
	}
	var body any = map[string]bool{"cancel": true}
	if len(uploads) > 0 {
		body = map[string][]string{"files": uploads}
	}
	return chooserError(m.control(ctx, profile, http.MethodPost, "/chooser/"+id, body, nil))
}

func chooserError(err error) error {
	var ce *controlError
	if errors.As(err, &ce) {
		switch ce.status {
		case http.StatusNotFound:
			return ErrChooserClosed
		case http.StatusServiceUnavailable:
			return ErrNoPortal
		}
	}
	return err
}

// restartChoosers gives the profile's open choosers their whole time
// limit again: control changed hands, so whoever has it now gets the
// time to answer. It is called with m.mu held, so the call to the
// browser runs on its own; an older image, without the route, ignores it.
func (m *Manager) restartChoosers(profile string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.control(ctx, profile, http.MethodPost, "/chooser/restart", nil, nil); err != nil {
			m.log.Debug("restarting the file choosers' time limit", "profile", profile, "err", err)
		}
	}()
}

// HumanControl reports whether a person holds control of the session (the
// live view's Take control, or a handoff link's), without counting as
// their use of it.
func (m *Manager) HumanControl(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.findLocked(id)
	return l != nil && l.human && (l.sess.State == Running || l.sess.State == NeedsHelp)
}
