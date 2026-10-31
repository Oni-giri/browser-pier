package ui

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

type overviewData struct {
	Profiles []profileView
	Help     *profileView // the profile waiting longest for you
	Waiting  int
	Running  int
	Mem      memoryView
	Tokens   []auth.Token
	Handoffs struct {
		Count  int
		Median time.Duration
	}
	CDPExample string
	Base       string
}

// memoryView is the memory report as the Overview and Settings show it.
type memoryView struct {
	sessions.MemoryReport
	Host  int64
	Parts []memoryPart
}

type memoryPart struct {
	Name  string
	Bytes int64
	Color string
}

// partColors tell browsers apart in the memory bar; the needs-you one is orange.
var partColors = []string{"#2F6BFF", "#8AA8FF", "#16A34A", "#7C3AED", "#0EA5E9", "#A1A1AA"}

func (u *UI) memoryView(r *http.Request, views []profileView) memoryView {
	mv := memoryView{Host: u.o.HostMemory}
	if len(u.m.List()) > 0 {
		mv.MemoryReport = u.memory(r.Context())
	} else {
		mv.Limit = u.m.MemoryLimit()
	}
	help := map[string]bool{}
	for _, v := range views {
		help[v.Name] = v.Status.Key == "help"
	}
	browsers := slices.Clone(mv.Browsers)
	slices.SortFunc(browsers, func(a, b sessions.Usage) int { return cmp.Compare(b.Bytes, a.Bytes) })
	i := 0
	for _, b := range browsers {
		c := "#A1A1AA"
		switch {
		case help[b.Profile]:
			c = "#FF6A3D"
		case i < len(partColors):
			c = partColors[i]
			i++
		}
		mv.Parts = append(mv.Parts, memoryPart{Name: b.Profile, Bytes: b.Bytes, Color: c})
	}
	return mv
}

// spanned is the overview's data with a cell's grid width.
type spanned struct {
	overviewData
	Span string
}

func (u *UI) overview(w http.ResponseWriter, r *http.Request) {
	views, err := u.views(r.Context(), r)
	if err != nil {
		u.log.Error("listing profiles", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	d := overviewData{Profiles: views, Base: u.baseURL(r)}
	for i := range views {
		switch views[i].Status.Key {
		case "help":
			d.Waiting++
			d.Running++
			if d.Help == nil {
				d.Help = &views[i]
			}
		case "running", "starting":
			d.Running++
		}
	}
	d.Mem = u.memoryView(r, views)
	if toks, err := u.web.Tokens().List(r.Context()); err == nil {
		slices.SortFunc(toks, func(a, b auth.Token) int { return b.LastUsedAt.Compare(a.LastUsedAt) })
		d.Tokens = toks
	}
	d.Handoffs.Count, d.Handoffs.Median, err = u.handoffs.Stats(r.Context(), time.Now().AddDate(0, 0, -7))
	if err != nil {
		u.log.Warn("counting handoffs", "err", err)
	}
	d.CDPExample = "google"
	if len(views) > 0 {
		d.CDPExample = views[0].Name
	}
	if r.Header.Get("HX-Request") == "true" {
		u.fragment(w, http.StatusOK, "overview", "cells", d)
		return
	}
	u.render(w, r, http.StatusOK, "overview", "Overview", "overview", d)
}
