package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Caps on what the browser container may send. It isn't trusted: a page
// may have taken it over.
const (
	// MaxSegment caps a segment file, as maxFrame caps a live frame.
	MaxSegment = 8 << 20
	// MaxPerMinute caps what one session may write in a minute.
	MaxPerMinute = 24 << 20
	// maxInit caps the stream's ftyp and moov together, maxMoof a
	// fragment's header.
	maxInit = 256 << 10
	maxMoof = 256 << 10
)

var errStream = errors.New("the recording stream isn't the MP4 it should be")

// readBox reads one top-level ISO BMFF box, header included, of at most
// limit bytes. Only its size and type are looked at.
func readBox(r io.Reader, limit int64) (typ string, box []byte, err error) {
	var h [16]byte
	if _, err := io.ReadFull(r, h[:8]); err != nil {
		return "", nil, err
	}
	size := int64(binary.BigEndian.Uint32(h[:4]))
	head := 8
	if size == 1 { // a 64-bit size follows the type
		if _, err := io.ReadFull(r, h[8:16]); err != nil {
			return "", nil, unexpected(err)
		}
		large := binary.BigEndian.Uint64(h[8:16])
		if large > uint64(limit) { //nolint:gosec // limit is a positive cap
			return "", nil, fmt.Errorf("%w: a box of %d bytes, over the %d cap", errOverCap, large, limit)
		}
		size, head = int64(large), 16 //nolint:gosec // checked against limit just above
	}
	typ = string(h[4:8])
	switch {
	case size < int64(head): // 0 ("to the end") included
		return "", nil, fmt.Errorf("%w: a box of size %d", errStream, size)
	case size > limit:
		return "", nil, fmt.Errorf("%w: a %q box of %d bytes, over the %d cap", errOverCap, sanitize(typ), size, limit)
	}
	box = make([]byte, size)
	copy(box, h[:head])
	if _, err := io.ReadFull(r, box[head:]); err != nil {
		return "", nil, unexpected(err)
	}
	return typ, box, nil
}

var errOverCap = errors.New("the recording stream went over a cap")

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// sanitize keeps a box type printable for a log line.
func sanitize(typ string) string {
	b := []byte(typ)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// maxSkipped is how many skippable boxes in a row a stream may send.
const maxSkipped = 8

// skippable boxes may come between the ones that matter; they aren't kept.
func skippable(typ string) bool {
	switch typ {
	case "free", "skip", "mfra", "sidx", "styp":
		return true
	}
	return false
}

// readInit reads the stream's head: ftyp, then moov.
func readInit(r io.Reader) ([]byte, error) {
	var out []byte
	want := "ftyp"
	for skipped := 0; ; skipped++ {
		if skipped > maxSkipped {
			return nil, fmt.Errorf("%w: more than %d boxes to skip in a row", errStream, maxSkipped)
		}
		typ, box, err := readBox(r, maxInit-int64(len(out)))
		if err != nil {
			return nil, err
		}
		switch {
		case typ == want:
			skipped = -1
			out = append(out, box...)
			if want == "moov" {
				return out, nil
			}
			want = "moov"
		case skippable(typ) && want == "moov":
		default:
			return nil, fmt.Errorf("%w: %q where %s should be", errStream, sanitize(typ), want)
		}
	}
}

// fragment is one moof and its mdat: about one frame, whole on its own.
type fragment struct {
	data    []byte
	payload int // the mdat's size less its header
}

// readFragment reads the next moof and the mdat after it, of at most
// limit bytes together.
func readFragment(r io.Reader, limit int64) (fragment, error) {
	for skipped := 0; ; skipped++ {
		typ, moof, err := readBox(r, min(limit, maxMoof))
		if err != nil {
			return fragment{}, err
		}
		if skippable(typ) {
			if skipped >= maxSkipped {
				return fragment{}, fmt.Errorf("%w: more than %d boxes to skip in a row", errStream, maxSkipped)
			}
			continue
		}
		if typ != "moof" {
			return fragment{}, fmt.Errorf("%w: %q where moof should be", errStream, sanitize(typ))
		}
		typ, mdat, err := readBox(r, limit-int64(len(moof)))
		if err != nil {
			return fragment{}, unexpected(err)
		}
		if typ != "mdat" {
			return fragment{}, fmt.Errorf("%w: %q where mdat should be", errStream, sanitize(typ))
		}
		head := 8
		if binary.BigEndian.Uint32(mdat[:4]) == 1 {
			head = 16
		}
		return fragment{data: append(moof, mdat...), payload: len(mdat) - head}, nil
	}
}
