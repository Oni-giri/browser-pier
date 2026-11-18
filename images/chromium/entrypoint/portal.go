package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

// The file chooser portal on the session bus (PROJ-152). It owns
// org.freedesktop.portal.Desktop before Chromium starts, and serves
// org.freedesktop.portal.FileChooser only: Chromium uses the portal for
// its file choosers when that name has an owner whose FileChooser
// version is 3 or more (no switch or environment variable needed; see
// images/chromium/ANTI-BOT.md). Every other interface on the object
// answers D-Bus's standard unknown-interface error, its properties
// included, so Chromium's other portal lookups (Settings, Notification,
// the host registry…) find nothing and it carries on as without a bus.

const (
	portalName      = "org.freedesktop.portal.Desktop"
	portalPath      = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	fileChooserIntf = "org.freedesktop.portal.FileChooser"
	requestIntf     = "org.freedesktop.portal.Request"
	propertiesIntf  = "org.freedesktop.DBus.Properties"
	// portalVersion is the FileChooser version reported: 4 has the
	// directory option, which is how a folder chooser is told and refused.
	portalVersion uint32 = 4
)

// portal is the portal on the bus. A nil portal is one that couldn't
// start: Chromium then shows its own dialogs, and the control socket's
// /chooser answers 503.
type portal struct {
	conn    *dbus.Conn
	chooser *chooser
	check   *browserCheck
	log     *slog.Logger
	mu      sync.Mutex
	closed  bool
}

// startPortal connects to the bus at addr and takes the portal's name,
// refusing to share it or be replaced.
func startPortal(log *slog.Logger, addr string, timeout time.Duration, focus focuser) (*portal, error) {
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, fmt.Errorf("connect to the session bus: %w", err)
	}
	p := &portal{conn: conn, check: &browserCheck{proc: procTree{root: "/proc"}}, log: log}
	p.chooser = newChooser(log, p, p.allow, focus, timeout)
	if err := conn.Export(fileChooser{p.chooser}, portalPath, fileChooserIntf); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.Export(properties{}, portalPath, propertiesIntf); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Requests of a caller that leaves the bus end with it.
	if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged")); err != nil {
		_ = conn.Close()
		return nil, err
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	go func() {
		for s := range signals {
			if s.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(s.Body) != 3 {
				continue
			}
			name, _ := s.Body[0].(string)
			newOwner, _ := s.Body[2].(string)
			if newOwner == "" && len(name) > 0 && name[0] == ':' {
				p.chooser.gone(name)
			}
		}
	}()
	reply, err := conn.RequestName(portalName, dbus.NameFlagDoNotQueue)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("own %s: %w", portalName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		_ = conn.Close()
		return nil, fmt.Errorf("own %s: someone else has it", portalName)
	}
	return p, nil
}

// setBrowser names the Chromium the portal answers, once started.
func (p *portal) setBrowser(pid int) {
	if p == nil {
		return
	}
	if err := p.check.setBrowser(pid); err != nil {
		p.log.Warn("the file chooser portal can't see the browser; it refuses every request", "err", err)
	}
}

// browserExited ends every request, and refuses new ones.
func (p *portal) browserExited() {
	if p == nil {
		return
	}
	p.check.browserExited()
	p.chooser.browserExited()
}

// close ends every request and leaves the bus.
func (p *portal) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.browserExited()
	_ = p.conn.Close()
}

var fallbackOnce sync.Once

func closeFD(fd int) error { return syscall.Close(fd) }

// allow is the caller check on a sender's credentials from the bus.
func (p *portal) allow(sender string) error {
	var creds map[string]dbus.Variant
	if err := p.conn.BusObject().Call("org.freedesktop.DBus.GetConnectionCredentials", 0, sender).Store(&creds); err != nil {
		return fmt.Errorf("the caller's credentials: %w", err)
	}
	c := callerCreds{pidfd: -1}
	if v, ok := creds["ProcessID"]; ok {
		if pid, ok := v.Value().(uint32); ok {
			c.pid = int(pid)
		}
	}
	if v, ok := creds["ProcessFD"]; ok {
		if fd, ok := v.Value().(dbus.UnixFD); ok {
			c.pidfd = int(fd)
			defer func() { _ = closeFD(c.pidfd) }()
		}
	}
	if c.pidfd < 0 {
		fallbackOnce.Do(func() {
			p.log.Info("the session bus gives no pidfd (a kernel before 6.5): the file chooser portal checks its callers by pid and start time")
		})
	}
	return p.check.allow(c)
}

// respond sends a request's Response to the caller that asked, and only
// to it.
func (p *portal) respond(sender string, path dbus.ObjectPath, code uint32, results map[string]dbus.Variant) error {
	msg := &dbus.Message{
		Type: dbus.TypeSignal,
		Headers: map[dbus.HeaderField]dbus.Variant{
			dbus.FieldPath:        dbus.MakeVariant(path),
			dbus.FieldInterface:   dbus.MakeVariant(requestIntf),
			dbus.FieldMember:      dbus.MakeVariant("Response"),
			dbus.FieldDestination: dbus.MakeVariant(sender),
		},
		Body: []any{code, results},
	}
	msg.Headers[dbus.FieldSignature] = dbus.MakeVariant(dbus.SignatureOf(msg.Body...))
	call := p.conn.Send(msg, nil)
	return call.Err
}

func (p *portal) exportRequest(path dbus.ObjectPath, close func(sender string) *dbus.Error) error {
	return p.conn.Export(request{close}, path, requestIntf)
}

func (p *portal) unexportRequest(path dbus.ObjectPath) {
	_ = p.conn.Export(nil, path, requestIntf)
}

// fileChooser is org.freedesktop.portal.FileChooser's methods.
type fileChooser struct{ c *chooser }

func (f fileChooser) OpenFile(sender dbus.Sender, _ string, _ string, opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	return f.c.openFile(string(sender), opts)
}

func (f fileChooser) SaveFile(sender dbus.Sender, _ string, _ string, opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	return f.c.saveFile(string(sender), opts)
}

// SaveFiles (saving several files into a folder) is refused.
func (f fileChooser) SaveFiles(dbus.Sender, string, string, map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	return "", portalRefused("saving several files isn't supported")
}

// request is a request object's Close.
type request struct {
	close func(sender string) *dbus.Error
}

func (r request) Close(sender dbus.Sender) *dbus.Error { return r.close(string(sender)) }

// properties serves the FileChooser's one property, version, and the
// standard errors for anything else.
type properties struct{}

func unknownInterface(intf string) *dbus.Error {
	e := dbus.MakeUnknownInterfaceError(intf)
	return &e
}

func (properties) Get(intf, name string) (dbus.Variant, *dbus.Error) {
	if intf != fileChooserIntf {
		return dbus.Variant{}, unknownInterface(intf)
	}
	if name != "version" {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{"no property " + name})
	}
	return dbus.MakeVariant(portalVersion), nil
}

func (properties) GetAll(intf string) (map[string]dbus.Variant, *dbus.Error) {
	if intf != fileChooserIntf {
		return nil, unknownInterface(intf)
	}
	return map[string]dbus.Variant{"version": dbus.MakeVariant(portalVersion)}, nil
}

func (properties) Set(intf, name string, _ dbus.Variant) *dbus.Error {
	if intf != fileChooserIntf {
		return unknownInterface(intf)
	}
	return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", []any{name + " is read-only"})
}

// registerChooser serves the control socket's chooser routes:
//
//	GET  /chooser        the open file choosers: id, multiple, accept, age_ms
//	POST /chooser/{id}   {"files": ["name", …]} or {"cancel": true}
//
// Both answer 503 when the portal isn't running.
func registerChooser(mux *http.ServeMux, p *portal) {
	off := func(w http.ResponseWriter) bool {
		if p == nil {
			http.Error(w, "the file chooser portal isn't running in this browser", http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	mux.HandleFunc("GET /chooser", func(w http.ResponseWriter, _ *http.Request) {
		if off(w) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choosers": p.chooser.list()})
	})
	mux.HandleFunc("POST /chooser/{id}", func(w http.ResponseWriter, r *http.Request) {
		if off(w) {
			return
		}
		var in struct {
			Files  []string `json:"files"`
			Cancel bool     `json:"cancel"`
		}
		if !decode(w, r, &in) {
			return
		}
		var err error
		switch {
		case in.Cancel && len(in.Files) > 0:
			http.Error(w, "send files or cancel, not both", http.StatusBadRequest)
			return
		case in.Cancel:
			err = p.chooser.cancel(r.PathValue("id"))
		default:
			err = p.chooser.answer(r.PathValue("id"), in.Files)
		}
		var ae *answerError
		switch {
		case errors.As(err, &ae):
			http.Error(w, ae.msg, ae.status)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
}
