package term

import (
	"errors"
	"os"
	"strings"
	"testing"

	"go.rockorager.dev/vaxis"
	"go.rockorager.dev/vaxis/ansi"
)

// The child's probe, its frame, and the delete that frees the frame's id. These
// are the exact bytes terminal-browser sends.
const (
	passthroughProbe   = "\x1b_Gi=4207,a=q,t=d,f=24,s=1,v=1;AAAA\x1b\\"
	passthroughFrame   = "\x1b[1;1H\x1b_Ga=T,f=32,s=800,v=480,t=d,i=7,p=1,C=1,q=2;AAAA\x1b\\"
	passthroughDelete7 = "\x1b_Ga=d,d=I,i=7,q=2\x1b\\"
	// The same frame without q=, so the widget's answer to it -- OK or an
	// error -- is actually written back. q=2 suppresses both.
	passthroughFrameLoud = "\x1b[1;1H\x1b_Ga=T,f=32,s=800,v=480,t=d,i=7,p=1,C=1;AAAA\x1b\\"
)

type fakeTransmit struct {
	controls string
	payload  string
}

type fakePlacement struct {
	col  int
	row  int
	pid  uint32
	cols int
	rows int
	keys string
}

// fakeKittyRelay records what the widget asked the host to do. refuseTransmit
// and refusePlace make it answer the way the real relay answers a fragment it
// cannot splice into an escape code: an error, and nothing recorded -- the
// image keeps the bytes it had and no placement is queued. placeAttempts counts
// the calls whatever the answer, so a frame that stops being drawn after a
// refusal is distinguishable from one that carries on.
type fakeKittyRelay struct {
	id             uint64
	refuseTransmit error
	refusePlace    error
	transmits      []fakeTransmit
	placements     []fakePlacement
	placeAttempts  int
	destroyed      int
}

func (r *fakeKittyRelay) ID() uint64 { return r.id }

func (r *fakeKittyRelay) SetTransmit(controls, payload string) error {
	if r.refuseTransmit != nil {
		return r.refuseTransmit
	}
	r.transmits = append(r.transmits, fakeTransmit{controls: controls, payload: payload})
	return nil
}

func (r *fakeKittyRelay) Place(win vaxis.Window, pid uint32, cols, rows int, placementKeys string) error {
	r.placeAttempts += 1
	if r.refusePlace != nil {
		return r.refusePlace
	}
	col, row := win.Origin()
	r.placements = append(r.placements, fakePlacement{
		col:  col,
		row:  row,
		pid:  pid,
		cols: cols,
		rows: rows,
		keys: placementKeys,
	})
	return nil
}

func (r *fakeKittyRelay) Destroy() { r.destroyed++ }

type fakeKittyHost struct {
	kitty          bool
	refuseTransmit error
	refusePlace    error
	relays         []*fakeKittyRelay
}

func (h *fakeKittyHost) CanKittyGraphics() bool { return h.kitty }

func (h *fakeKittyHost) NewKittyRelay() kittyRelay {
	relay := &fakeKittyRelay{
		id:             uint64(4001 + len(h.relays)),
		refuseTransmit: h.refuseTransmit,
		refusePlace:    h.refusePlace,
	}
	h.relays = append(h.relays, relay)
	return relay
}

// A model with a pixel-aware size, so a frame sized in pixels has a cell
// geometry to be placed at (cells are 10x20 px here).
func newPassthroughModel(t *testing.T, host *fakeKittyHost) (*Model, *os.File) {
	t.Helper()
	vt, r := newReplyTestModel(t)
	vt.kittyHost = host
	vt.resize(80, 24)
	vt.size = vaxis.Resize{Cols: 80, Rows: 24, XPixel: 800, YPixel: 480}
	return vt, r
}

// A sender probes with a=q before it will use the protocol at all. The widget
// answers locally, without asking the host.
func TestKittyPassthroughQueryDirect(t *testing.T) {
	vt, r := newPassthroughModel(t, &fakeKittyHost{kitty: true})

	vt.WriteString(passthroughProbe)

	want := "\x1b_Gi=4207;OK\x1b\\"
	if got := readReply(t, r, len(want)); got != want {
		t.Fatalf("kitty query reply = %q, want %q", got, want)
	}
}

// With no kitty support on the host there is nothing to relay to, so the probe
// must be refused rather than silently dropped.
func TestKittyPassthroughQueryWithoutHostKitty(t *testing.T) {
	vt, r := newPassthroughModel(t, &fakeKittyHost{kitty: false})

	vt.WriteString(passthroughProbe)

	want := "\x1b_Gi=4207;ENOTSUP"
	if got := readReply(t, r, len(want)); got != want {
		t.Fatalf("kitty query reply = %q, want prefix %q", got, want)
	}
}

// A transmit-and-display is relayed to a host image with a host-allocated id,
// and placed at the widget's origin on the next Draw. Re-sending the same frame
// replaces the bytes in place rather than leaking a second host image.
func TestKittyPassthroughTransmitPlacesRemapped(t *testing.T) {
	host := &fakeKittyHost{kitty: true}
	vt, r := newPassthroughModel(t, host)

	vt.WriteString(passthroughFrame)
	assertNoReply(t, r)

	if len(host.relays) != 1 {
		t.Fatalf("relays = %d, want 1", len(host.relays))
	}
	relay := host.relays[0]
	if relay.id == 7 {
		t.Fatalf("relay id = %d, want an id remapped off the child's 7", relay.id)
	}
	if len(relay.transmits) != 1 {
		t.Fatalf("transmits = %d, want 1", len(relay.transmits))
	}
	if c := relay.transmits[0].controls; strings.Contains(c, "a=") || strings.Contains(c, "i=") {
		t.Fatalf("transmit controls = %q, want no a= or i= key", c)
	}
	if got := relay.transmits[0].payload; got != "AAAA" {
		t.Fatalf("transmit payload = %q, want %q", got, "AAAA")
	}

	vt.Draw(vaxis.Window{Width: 80, Height: 24})

	if len(relay.placements) != 1 {
		t.Fatalf("placements = %d, want 1", len(relay.placements))
	}
	want := fakePlacement{col: 0, row: 0, pid: 1, cols: 80, rows: 24, keys: ""}
	if relay.placements[0] != want {
		t.Fatalf("placement = %+v, want %+v", relay.placements[0], want)
	}

	vt.WriteString(passthroughFrame)

	if len(host.relays) != 1 {
		t.Fatalf("relays after replace = %d, want 1", len(host.relays))
	}
	if len(relay.transmits) != 2 {
		t.Fatalf("transmits after replace = %d, want 2", len(relay.transmits))
	}
}

// a=d on the child's id must free the host image it was remapped to, and stop
// it being placed.
func TestKittyPassthroughDeleteFreesHostImage(t *testing.T) {
	host := &fakeKittyHost{kitty: true}
	vt, r := newPassthroughModel(t, host)

	vt.WriteString(passthroughFrame)
	vt.WriteString(passthroughDelete7)
	assertNoReply(t, r)

	if len(host.relays) != 1 {
		t.Fatalf("relays = %d, want 1", len(host.relays))
	}
	relay := host.relays[0]
	if relay.destroyed != 1 {
		t.Fatalf("destroyed = %d, want 1", relay.destroyed)
	}

	vt.Draw(vaxis.Window{Width: 80, Height: 24})

	if len(relay.placements) != 0 {
		t.Fatalf("placements = %d, want 0 after delete", len(relay.placements))
	}
}

// The host relay refuses a frame it cannot splice into an escape code, and a
// refusal leaves it holding the bytes of the PREVIOUS frame. Placing anyway
// would put that stale picture on screen as though this command had landed, and
// answering OK would leave the child with no reason to re-send: the command has
// to be dropped and the child told.
func TestKittyPassthroughRefusedTransmitIsDropped(t *testing.T) {
	host := &fakeKittyHost{kitty: true, refuseTransmit: errors.New("kitty payload holds '?' at byte 2")}
	vt, r := newPassthroughModel(t, host)

	vt.WriteString(passthroughFrameLoud)

	want := "\x1b_Gi=7,p=1;EINVAL"
	if got := readReply(t, r, len(want)); got != want {
		t.Fatalf("reply to a refused frame = %q, want prefix %q", got, want)
	}

	if len(host.relays) != 1 {
		t.Fatalf("relays = %d, want 1", len(host.relays))
	}
	relay := host.relays[0]
	if len(relay.transmits) != 0 {
		t.Fatalf("transmits = %d, want 0 for a refused frame", len(relay.transmits))
	}
	if got := len(vt.graphics); got != 0 {
		t.Fatalf("graphics = %d, want 0; a refused frame must not be placed", got)
	}

	vt.Draw(vaxis.Window{Width: 80, Height: 24})

	if relay.placeAttempts != 0 {
		t.Fatalf("place attempts = %d, want 0 for a refused frame", relay.placeAttempts)
	}
}

// A frame the relay does take is placed and answered OK, which is what makes
// the assertions above about the refused one mean something.
func TestKittyPassthroughAcceptedTransmitRepliesOK(t *testing.T) {
	host := &fakeKittyHost{kitty: true}
	vt, r := newPassthroughModel(t, host)

	vt.WriteString(passthroughFrameLoud)

	want := "\x1b_Gi=7,p=1;OK\x1b\\"
	if got := readReply(t, r, len(want)); got != want {
		t.Fatalf("reply to an accepted frame = %q, want %q", got, want)
	}

	vt.Draw(vaxis.Window{Width: 80, Height: 24})

	if len(host.relays[0].placements) != 1 {
		t.Fatalf("placements = %d, want 1", len(host.relays[0].placements))
	}
}

// A refused placement queues nothing on the host, so the image is simply absent
// from this frame. The rest of the frame is not: a Draw that gave up at the
// first refusal would take every later image down with it.
func TestKittyPassthroughRefusedPlaceDrawsTheRest(t *testing.T) {
	host := &fakeKittyHost{kitty: true, refusePlace: errors.New("kitty placement keys hold '-' at byte 2")}
	vt, _ := newPassthroughModel(t, host)

	// Two small images on different rows, so both are visible at once and
	// neither scrolls the other out of the frame.
	vt.WriteString("\x1b[1;1H\x1b_Ga=T,f=32,s=40,v=40,t=d,i=7,p=1,c=4,r=2,C=1,q=2;AAAA\x1b\\")
	vt.WriteString("\x1b[9;1H\x1b_Ga=T,f=32,s=40,v=40,t=d,i=8,p=1,c=4,r=2,C=1,q=2;AAAA\x1b\\")

	if len(host.relays) != 2 {
		t.Fatalf("relays = %d, want 2", len(host.relays))
	}

	vt.Draw(vaxis.Window{Width: 80, Height: 24})

	for i, relay := range host.relays {
		if relay.placeAttempts != 1 {
			t.Fatalf("relay %d place attempts = %d, want 1", i, relay.placeAttempts)
		}
		if len(relay.placements) != 0 {
			t.Fatalf("relay %d placements = %d, want 0 for a refused placement", i, len(relay.placements))
		}
	}
}

// An APC that is not a kitty graphics command must still reach the application
// as an event. The kitty arm is an addition, not a replacement, and a widget
// that swallowed every APC would break callers relying on EventAPC.
func TestNonKittyAPCStillPostsEvent(t *testing.T) {
	vt := New()
	vt.resize(80, 24)
	vt.events = make(chan vaxis.Event, 4)

	vt.update(ansi.APC{Data: "not-a-kitty-command"})

	select {
	case ev := <-vt.events:
		apc, ok := ev.(EventAPC)
		if !ok {
			t.Fatalf("event = %T, want EventAPC", ev)
		}
		if string(apc.Payload) != "not-a-kitty-command" {
			t.Fatalf("payload = %q, want %q", apc.Payload, "not-a-kitty-command")
		}
	default:
		t.Fatal("no EventAPC posted for a non-kitty APC")
	}
}
