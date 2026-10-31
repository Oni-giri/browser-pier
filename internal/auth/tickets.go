package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// CDP connect tickets let clients that can't set headers connect to
// /cdp/{profile} with ?ticket= instead of the agent token, so no
// long-lived secret sits in a URL a proxy in front might log. A ticket
// is single-use, lasts TicketTTL and opens one profile's CDP endpoint.
const (
	TicketPrefix = "bp_cdp_"
	TicketTTL    = 60 * time.Second

	maxTicketsPerToken = 1000
)

// ErrTooManyTickets: a token has too many unused tickets.
var ErrTooManyTickets = errors.New("too many unused CDP tickets for this token; use them or let them expire")

type ticket struct {
	tokenID, profile string
	expires          time.Time
}

// tickets are kept in memory only, by hash: a restart voids them, which
// at a minute's lifetime costs nothing.
type tickets struct {
	mu  sync.Mutex
	m   map[string]ticket // by hashToken(secret)
	now func() time.Time
}

func newTickets() *tickets { return &tickets{m: map[string]ticket{}, now: time.Now} }

func (t *tickets) mint(tokenID, profile string) (string, time.Time, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	n := 0
	for h, k := range t.m {
		switch {
		case !now.Before(k.expires):
			delete(t.m, h)
		case k.tokenID == tokenID:
			n++
		}
	}
	if n >= maxTicketsPerToken {
		return "", time.Time{}, ErrTooManyTickets
	}
	secret := TicketPrefix + randomString(16) // 128 bits
	expires := now.Add(TicketTTL)
	t.m[hashToken(secret)] = ticket{tokenID: tokenID, profile: profile, expires: expires}
	return secret, expires, nil
}

// redeem uses the ticket up, valid or not, so the first caller wins.
func (t *tickets) redeem(secret string) (ticket, bool) {
	h := hashToken(secret)
	t.mu.Lock()
	k, ok := t.m[h]
	delete(t.m, h)
	t.mu.Unlock()
	return k, ok && t.now().Before(k.expires)
}

// MintCDPTicket makes a ticket that connects as tok to profile's CDP
// endpoint, once, before it expires.
func (w *Web) MintCDPTicket(tok Token, profile string) (string, time.Time, error) {
	return w.tickets.mint(tok.ID, profile)
}

// ByCDPTicket reports whether the request authenticated with a CDP ticket.
func ByCDPTicket(ctx context.Context) bool {
	by, _ := ctx.Value(ticketKey).(bool)
	return by
}

// RequireTokenOrCDPTicket guards /cdp/{profile}: a ?ticket= for that
// profile, or the token in the Authorization header as RequireToken
// takes it. A token in the URL is refused.
func (w *Web) RequireTokenOrCDPTicket(next http.Handler) http.Handler {
	byToken := w.RequireToken(next)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Has("token") {
			jsonError(rw, http.StatusBadRequest, "don't put the token in the URL: send it as Authorization: Bearer, or get a ticket from POST /api/sessions/{id}/cdp-ticket and connect with ?ticket=")
			return
		}
		if !q.Has("ticket") {
			byToken.ServeHTTP(rw, r)
			return
		}
		ip := w.ClientIP(r)
		if wait := w.badToken.Locked(ip); wait > 0 {
			retryAfter(rw, wait)
			jsonError(rw, http.StatusTooManyRequests, "too many bad tokens from this address; try again later")
			return
		}
		k, ok := w.tickets.redeem(q.Get("ticket"))
		var t Token
		if ok && k.profile == r.PathValue("profile") {
			var err error
			t, err = w.tokens.Get(r.Context(), k.tokenID)
			if err != nil && !errors.Is(err, ErrTokenNotFound) {
				w.log.Error("checking a CDP ticket's token", "err", err)
				jsonError(rw, http.StatusInternalServerError, "couldn't check the ticket")
				return
			}
			ok = err == nil // not found: revoked since, which voids its tickets
		} else {
			ok = false
		}
		if !ok {
			if a, _ := w.badToken.Begin(ip); a != nil {
				a.Fail()
			}
			w.log.Warn("CDP ticket rejected", "ip", ip)
			jsonError(rw, http.StatusUnauthorized, "this ticket is unknown, used, expired or for another profile; get a new one from POST /api/sessions/{id}/cdp-ticket")
			return
		}
		q.Del("ticket")
		ctx := context.WithValue(context.WithValue(r.Context(), tokenKey, t), ticketKey, true)
		r = r.Clone(ctx)
		r.URL.RawQuery = q.Encode()
		next.ServeHTTP(rw, r)
	})
}
