package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// later runs f after d, in the background, as the browser would write
// while an action's notice waits; the test waits for it before ending.
func later(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(d)
		f()
	}()
	t.Cleanup(func() { <-done })
}

func TestDownloadsNewFinishedFile(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	write(t, filepath.Join(dl, "old.csv"), "old", time.Hour)
	m := s.Mark("google", "")
	write(t, filepath.Join(dl, "report.csv"), strings.Repeat("x", 12345), 0)
	start := time.Now()
	got := m.Downloads(context.Background(), 3*time.Second)
	if want := []Download{{Name: "report.csv", Size: 12345, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("a finished download waited %v", time.Since(start))
	}
	if l := Lines(got); len(l) != 1 || l[0] != "downloaded report.csv (12 kB)" {
		t.Errorf("Lines = %q", l)
	}
	if got[0].LogLine() != "Downloaded report.csv (12 kB)" {
		t.Errorf("LogLine = %q", got[0].LogLine())
	}
}

func TestDownloadsNoChangeDoesntWait(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	write(t, filepath.Join(dl, "old.csv"), "old", time.Hour)
	// A download left under way before the action isn't this action's:
	// it is neither waited for nor reported.
	write(t, filepath.Join(dl, "stale.zip.crdownload"), "partial", time.Hour)
	m := s.Mark("google", "")
	start := time.Now()
	if got := m.Downloads(context.Background(), 3*time.Second); len(got) != 0 {
		t.Fatalf("Downloads = %+v, want nothing", got)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("no download took %v", d)
	}
}

func TestDownloadsPartialFinishesWithinTheWait(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	// Chrome's way: "Unconfirmed N.crdownload", renamed once it knows the
	// name, then renamed again once it is finished.
	write(t, filepath.Join(dl, "Unconfirmed 4242.crdownload"), "a,b", 0)
	later(t, 100*time.Millisecond, func() {
		_ = os.Rename(filepath.Join(dl, "Unconfirmed 4242.crdownload"), filepath.Join(dl, "export.csv.crdownload"))
	})
	later(t, 200*time.Millisecond, func() {
		f, _ := os.OpenFile(filepath.Join(dl, "export.csv.crdownload"), os.O_APPEND|os.O_WRONLY, 0)
		_, _ = f.WriteString("\n1,2\n")
		_ = f.Close()
		_ = os.Rename(filepath.Join(dl, "export.csv.crdownload"), filepath.Join(dl, "export.csv"))
	})
	start := time.Now()
	got := m.Downloads(context.Background(), 3*time.Second)
	if want := []Download{{Name: "export.csv", Size: 8, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("it waited %v for a download that finished at 200 ms", d)
	}
}

func TestDownloadsStillGrowingAtTheDeadline(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	part := filepath.Join(dl, "big.bin.crdownload")
	write(t, part, strings.Repeat("x", 4100000), 0)
	stop := make(chan struct{})
	later(t, 0, func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			f, _ := os.OpenFile(part, os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("more")
			_ = f.Close()
		}
	})
	start := time.Now()
	got := m.Downloads(context.Background(), 300*time.Millisecond)
	close(stop)
	if d := time.Since(start); d < 250*time.Millisecond || d > time.Second {
		t.Errorf("waited %v, want about 300 ms", d)
	}
	if len(got) != 1 || got[0].State != InProgress || got[0].Name != "big.bin" || got[0].Size <= 4100000 {
		t.Fatalf("Downloads = %+v, want big.bin in progress, larger than it started", got)
	}
	if l := Lines(got)[0]; !strings.HasPrefix(l, "download in progress: big.bin (4.1 MB so far); list_files to check later") {
		t.Errorf("line %q", l)
	}
	if got[0].LogLine() != "" {
		t.Errorf("an unfinished download is logged: %q", got[0].LogLine())
	}
}

func TestDownloadsPartialRemoved(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	write(t, filepath.Join(dl, "cancelled.zip.crdownload"), "part", 0)
	later(t, 100*time.Millisecond, func() { _ = os.Remove(filepath.Join(dl, "cancelled.zip.crdownload")) })
	start := time.Now()
	got := m.Downloads(context.Background(), 3*time.Second)
	if len(got) != 1 || got[0].State != Vanished {
		t.Fatalf("Downloads = %+v, want one vanished", got)
	}
	if l := Lines(got); l[0] != "a download vanished before it finished" {
		t.Errorf("line %q", l[0])
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("it waited %v after the download went", d)
	}
}

// A listing taken while Chrome renames a partial download to its name can
// hold neither: the notice looks once more before saying it vanished.
func TestDownloadsRenameBetweenListingsIsntVanished(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	write(t, filepath.Join(dl, "report.csv.crdownload"), "part", 0)
	later(t, 100*time.Millisecond, func() { _ = os.Remove(filepath.Join(dl, "report.csv.crdownload")) })
	// The finished file comes just after a listing that saw neither.
	m.beforeSettle = func() { write(t, filepath.Join(dl, "report.csv"), "whole", 0) }
	got := m.Downloads(context.Background(), 3*time.Second)
	if want := []Download{{Name: "report.csv", Size: 5, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
}

// The files cap's eviction can remove a finished download between two
// listings: the notice says so, and nothing fails.
func TestDownloadsFinishedThenEvicted(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	write(t, filepath.Join(dl, "a.csv"), "aaa", 0)
	write(t, filepath.Join(dl, "b.zip.crdownload"), "b", 0)
	later(t, 60*time.Millisecond, func() { _ = os.Remove(filepath.Join(dl, "a.csv")) })
	later(t, 150*time.Millisecond, func() { _ = os.Rename(filepath.Join(dl, "b.zip.crdownload"), filepath.Join(dl, "b.zip")) })
	got := m.Downloads(context.Background(), 3*time.Second)
	want := []Download{{Name: "a.csv", Size: 3, State: Removed}, {Name: "b.zip", Size: 1, State: Downloaded}}
	if len(got) != 2 {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
	byName := map[string]Download{got[0].Name: got[0], got[1].Name: got[1]}
	for _, w := range want {
		if byName[w.Name] != w {
			t.Errorf("%s: %+v, want %+v", w.Name, byName[w.Name], w)
		}
	}
	if l := (Download{Name: "a.csv", Size: 3, State: Removed}).Line(); !strings.HasPrefix(l, "downloaded a.csv (3 B), then removed") {
		t.Errorf("line %q", l)
	}
}

// A download already under way before the action that finishes during
// it is reported: its finished name is new.
func TestDownloadsEarlierPartialFinishes(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	write(t, filepath.Join(dl, "slow.iso.crdownload"), "12345", 0)
	m := s.Mark("google", "")
	if err := os.Rename(filepath.Join(dl, "slow.iso.crdownload"), filepath.Join(dl, "slow.iso")); err != nil {
		t.Fatal(err)
	}
	got := m.Downloads(context.Background(), 3*time.Second)
	if want := []Download{{Name: "slow.iso", Size: 5, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
}

func TestDownloadNamesCleanedAndLinksIgnored(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	write(t, filepath.Join(dir, "secret"), "outside", 0)
	m := s.Mark("google", "")
	// Bidi override (an "exe" that reads as "txt"), zero-width space, a
	// tag character and a line break.
	write(t, filepath.Join(dl, "inv\u202eoice\u200b\U000E0041\nnext.csv"), "x", 0)
	if err := os.Symlink(filepath.Join(dir, "secret"), filepath.Join(dl, "planted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "secret"), filepath.Join(dl, "planted.crdownload")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dl, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := m.Downloads(context.Background(), 3*time.Second)
	if want := []Download{{Name: "invoice next.csv", Size: 1, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Errorf("a planted partial link was waited for")
	}
}

// The downloads folder replaced by a link: os.Root refuses it, and the
// notice is empty rather than an error.
func TestDownloadsFolderLinkRefused(t *testing.T) {
	s, dir := setup(t, 0)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "host.txt"), "host", 0)
	if err := os.RemoveAll(filepath.Join(dir, Downloads)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, Downloads)); err != nil {
		t.Fatal(err)
	}
	m := s.Mark("google", "")
	write(t, filepath.Join(outside, "new.txt"), "new", 0)
	if got := m.Downloads(context.Background(), time.Second); len(got) != 0 {
		t.Fatalf("Downloads through a link = %+v", got)
	}
	var nilMark *Mark
	if got := nilMark.Downloads(context.Background(), time.Second); got != nil {
		t.Fatalf("nil Mark = %+v", got)
	}
}

func TestDownloadsCallerGoneStopsTheWait(t *testing.T) {
	s, dir := setup(t, 0)
	m := s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	write(t, filepath.Join(dir, Downloads, "x.bin.crdownload"), "x", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := m.Downloads(ctx, 10*time.Second)
	if d := time.Since(start); d > time.Second {
		t.Errorf("a cancelled wait went on for %v", d)
	}
	if len(got) != 1 || got[0].State != InProgress {
		t.Errorf("Downloads = %+v, want it in progress", got)
	}
}

func TestLinesCap(t *testing.T) {
	var ds []Download
	for i := range 8 {
		ds = append(ds, Download{Name: string(rune('a'+i)) + ".txt", Size: int64(i), State: Downloaded})
	}
	l := Lines(ds)
	if len(l) != 6 || l[5] != "and 3 more" || l[0] != "downloaded a.txt (0 B)" {
		t.Fatalf("Lines = %q", l)
	}
}

// The activity log names at most MaxNotices finished files of a notice,
// then counts the rest; unfinished ones aren't logged.
func TestLogLinesCap(t *testing.T) {
	var ds []Download
	for i := range 8 {
		ds = append(ds, Download{Name: fmt.Sprintf("f%d.csv", i), Size: 1, State: Downloaded})
	}
	ds = append(ds, Download{Name: "big.iso", Size: 9, State: InProgress})
	got := LogLines(ds)
	want := []string{"Downloaded f0.csv (1 B)", "Downloaded f1.csv (1 B)", "Downloaded f2.csv (1 B)", "Downloaded f3.csv (1 B)", "Downloaded f4.csv (1 B)", "Downloaded 3 more files"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LogLines = %q, want %q", got, want)
	}
	if got := LogLines(ds[2:]); len(got) != 6 || got[5] != "Downloaded 1 more file" {
		t.Errorf("LogLines of 6 = %q", got)
	}
	if got := LogLines(ds[3:]); len(got) != 5 || got[4] != "Downloaded f7.csv (1 B)" {
		t.Errorf("LogLines of 5 = %q", got)
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 4100: "4.1 kB", 12345: "12 kB", 999499: "999 kB", 999500: "1.0 MB", 4100000: "4.1 MB", 5 << 30: "5.4 GB"} {
		if got := Size(n); got != want {
			t.Errorf("Size(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestDownloadWait(t *testing.T) {
	ms := func(n int) *int { return &n }
	for _, c := range []struct {
		in   *int
		want time.Duration
		ok   bool
	}{{nil, 3 * time.Second, true}, {ms(0), 0, true}, {ms(30000), 30 * time.Second, true}, {ms(30001), 0, false}, {ms(-1), 0, false}} {
		got, err := DownloadWait(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("DownloadWait(%v) = %v, %v", c.in, got, err)
		}
	}
}

// Chrome creates the file only once the server has answered, which can
// be just after the action: the notice keeps looking for StartGrace.
func TestDownloadsLookForALateStart(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.grace = 300 * time.Millisecond
	m.Ended()
	later(t, 100*time.Millisecond, func() { write(t, filepath.Join(dl, "late.csv"), "late", 0) })
	got := m.Downloads(context.Background(), 3*time.Second)
	if want := []Download{{Name: "late.csv", Size: 4, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
	// Nothing at all: it looks for StartGrace from the end of the action,
	// no longer.
	m = s.Mark("google", "")
	m.Ended()
	time.Sleep(StartGrace)
	start := time.Now()
	if got := m.Downloads(context.Background(), 3*time.Second); len(got) != 0 {
		t.Fatalf("Downloads = %+v", got)
	}
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Errorf("the grace was over, yet it looked for %v", d)
	}
}

// A caller that set download_wait_ms expects a download: the notice looks
// that long for one to start, as a remote server's answer comes later
// than StartGrace; without it, a late start waits for the next action.
func TestDownloadsExpectedLooksLongerForAStart(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.Ended()
	later(t, 200*time.Millisecond, func() { write(t, filepath.Join(dl, "slow.csv"), "slow", 0) })
	if got := m.Downloads(context.Background(), 3*time.Second); len(got) != 0 {
		t.Fatalf("without Expect, Downloads = %+v, want nothing yet", got)
	}
	time.Sleep(300 * time.Millisecond) // slow.csv is there before the next mark
	m = s.Mark("google", "")
	m.Expect(time.Second)
	m.Ended()
	later(t, 200*time.Millisecond, func() { write(t, filepath.Join(dl, "slower.csv"), "slower", 0) })
	start := time.Now()
	got := m.Downloads(context.Background(), time.Second)
	if want := []Download{{Name: "slower.csv", Size: 6, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with Expect, Downloads = %+v, want %+v", got, want)
	}
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Errorf("it kept looking for %v after the download came", d)
	}
	// Nothing comes: it looks for the whole wait, no longer.
	m = s.Mark("google", "")
	m.Expect(300 * time.Millisecond)
	m.Ended()
	start = time.Now()
	if got := m.Downloads(context.Background(), 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("Downloads = %+v", got)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 600*time.Millisecond {
		t.Errorf("it looked for %v, want about 300 ms", d)
	}
	// 0 (don't wait) keeps StartGrace.
	m = s.Mark("google", "")
	m.Expect(0)
	if m.grace != StartGrace {
		t.Errorf("Expect(0): grace %v", m.grace)
	}
}

// What finished after a session's notice, before its next action, is
// told with that action, once; a new session starts afresh.
func TestDownloadsEarlierTold(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "s1")
	write(t, filepath.Join(dl, "Unconfirmed 99.crdownload"), "part", 0)
	got := m.Downloads(context.Background(), 0)
	if want := []Download{{Name: "", Size: 4, State: InProgress}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
	if l := got[0].Line(); l != "download in progress (4 B so far; it gets its name when it finishes); list_files to check later" {
		t.Errorf("line %q", l)
	}
	// Then it finishes, and something else arrives, between actions.
	if err := os.Rename(filepath.Join(dl, "Unconfirmed 99.crdownload"), filepath.Join(dl, "big.bin")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dl, "page.pdf"), "pdf!!", 0)
	m = s.Mark("google", "s1")
	write(t, filepath.Join(dl, "now.csv"), "now", 0)
	got = m.Downloads(context.Background(), time.Second)
	if len(got) != 3 || !got[0].Earlier || !got[1].Earlier || got[2] != (Download{Name: "now.csv", Size: 3, State: Downloaded}) {
		t.Fatalf("Downloads = %+v", got)
	}
	names := got[0].Name + "," + got[1].Name
	if names != "big.bin,page.pdf" && names != "page.pdf,big.bin" {
		t.Errorf("earlier %s", names)
	}
	if l := got[0].Line(); !strings.HasSuffix(l, "), after the last action's answer") {
		t.Errorf("line %q", l)
	}
	// Told once.
	if got := s.Mark("google", "s1").Downloads(context.Background(), time.Second); len(got) != 0 {
		t.Fatalf("told again: %+v", got)
	}
	// Another session, or the same one forgotten, has nothing from before.
	write(t, filepath.Join(dl, "x.csv"), "x", 0)
	s.Forget("s1")
	for _, id := range []string{"s1", "s2"} {
		if got := s.Mark("google", id).Downloads(context.Background(), time.Second); len(got) != 0 {
			t.Errorf("%s: %+v", id, got)
		}
	}
}

// Chrome may write a finished download through a hidden temporary file:
// it is waited for, never named, and List doesn't show it.
func TestDownloadsHiddenTemporaryFile(t *testing.T) {
	s, dir := setup(t, 0)
	dl := filepath.Join(dir, Downloads)
	m := s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	tmp := filepath.Join(dl, ".org.chromium.Chromium.oGaKNr")
	write(t, tmp, "name,amount\n", 0)
	if list, err := s.List("google"); err != nil || len(list) != 0 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	later(t, 100*time.Millisecond, func() { _ = os.Rename(tmp, filepath.Join(dl, "export.csv")) })
	got := m.Downloads(context.Background(), 3*time.Second)
	if want := []Download{{Name: "export.csv", Size: 12, State: Downloaded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Downloads = %+v, want %+v", got, want)
	}
	// One left behind, or gone, says nothing.
	m = s.Mark("google", "")
	write(t, filepath.Join(dl, ".org.chromium.Chromium.x"), "x", 0)
	if got := m.Downloads(context.Background(), 0); len(got) != 0 {
		t.Fatalf("a temporary file at the deadline: %+v", got)
	}
	m = s.Mark("google", "")
	m.poll = 20 * time.Millisecond
	write(t, filepath.Join(dl, ".org.chromium.Chromium.y"), "y", 0)
	later(t, 60*time.Millisecond, func() { _ = os.Remove(filepath.Join(dl, ".org.chromium.Chromium.y")) })
	if got := m.Downloads(context.Background(), time.Second); len(got) != 0 {
		t.Fatalf("a temporary file gone: %+v", got)
	}
}
