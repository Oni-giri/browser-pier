package sessions

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
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
	// ErrNoPortal: the browser has no portal (an image from before it,
	// which answers 404, or 503).
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
		// An image from before the portal has no such route: a browser
		// kept running through an update, until it restarts.
		if ce := (*controlError)(nil); errors.As(err, &ce) && ce.status == http.StatusNotFound {
			return nil, ErrNoPortal
		}
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
// and the browser checks again), or cancels it when there are none. It
// is the person's input, for their takeover turn (HumanInput): the
// browser refuses it, with ErrControlEnded, once that takeover has ended
// (TakeoverHeader, PROJ-196).
func (m *Manager) AnswerChooser(ctx context.Context, profile, id string, uploads []string, turn uint64) error {
	if !chooserIDRE.MatchString(id) {
		return ErrChooserClosed
	}
	var body any = map[string]bool{"cancel": true}
	if len(uploads) > 0 {
		body = map[string][]string{"files": uploads}
	}
	hdr := map[string]string{TakeoverHeader: strconv.FormatUint(turn, 10)}
	return chooserError(m.controlWith(ctx, profile, http.MethodPost, "/chooser/"+id, hdr, body, nil))
}

func chooserError(err error) error {
	var ce *controlError
	if errors.As(err, &ce) {
		switch ce.status {
		case http.StatusNotFound:
			return ErrChooserClosed
		case http.StatusConflict:
			return ErrControlEnded // the browser fenced the takeover
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
