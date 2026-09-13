package vaxis

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// mustSetTransmit hands the relay a control string and a body the relay is
// expected to accept.
func mustSetTransmit(t *testing.T, r *KittyRelay, controls, payload string) {
	t.Helper()
	if err := r.SetTransmit(controls, payload); err != nil {
		t.Fatalf("SetTransmit(%q, ...) = %v, want no error", controls, err)
	}
}

// mustPlace queues a placement the relay is expected to accept.
func mustPlace(t *testing.T, r *KittyRelay, win Window, pid uint32, cols, rows int, placementKeys string) {
	t.Helper()
	if err := r.Place(win, pid, cols, rows, placementKeys); err != nil {
		t.Fatalf("Place(..., %q) = %v, want no error", placementKeys, err)
	}
}

// A relayed image replaces its predecessor in place: the bytes change under
// the same image id at the same cell, so the render writes a second a=T with
// no delete in between. Placing it again without new bytes writes nothing at
// all.
func TestKittyRelayTransmitsEveryNewGeneration(t *testing.T) {
	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)
	win := vx.Window()
	relay := vx.NewKittyRelay()

	mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")
	vx.graphicsNext = nil
	mustPlace(t, relay, win, 1, 2, 1, "")
	vx.render()
	if _, err := vx.tw.Flush(); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,a=T,i=%d,p=1,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
	if got := out.String(); !strings.Contains(got, want) {
		t.Fatalf("first frame = %q, want %q", got, want)
	}

	out.Reset()
	mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "BBBB")
	vx.graphicsNext = nil
	mustPlace(t, relay, win, 1, 2, 1, "")
	vx.render()
	if _, err := vx.tw.Flush(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	want = fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,a=T,i=%d,p=1,C=1,q=2,m=0;BBBB\x1b\\", relay.ID())
	if !strings.Contains(got, want) {
		t.Fatalf("second frame = %q, want %q", got, want)
	}
	if strings.Contains(got, "a=d") {
		t.Fatalf("second frame = %q, want no delete before the replacement", got)
	}

	out.Reset()
	vx.graphicsNext = nil
	mustPlace(t, relay, win, 1, 2, 1, "")
	vx.render()
	if _, err := vx.tw.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("third frame = %q, want no output for an unchanged placement", got)
	}
}

// SetTransmit runs on the goroutine reading the other process while placements
// are written during the render, so a generation and the bytes behind it have
// to become visible together. The other order lets the render transmit an
// image with no body and mark that generation sent, after which the bytes it
// was handed never reach the terminal at all.
func TestKittyRelayTransmitsNoGenerationWithoutItsBody(t *testing.T) {
	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)
	relay := vx.NewKittyRelay()
	mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")

	replaced := make(chan struct{})
	go func() {
		defer close(replaced)
		for i := 0; i < 20000; i++ {
			if err := relay.SetTransmit("f=32,s=8,v=8,t=d", "BBBB"); err != nil {
				t.Errorf("SetTransmit = %v, want no error", err)
				return
			}
		}
	}()

	bodyless := 0
	for done := false; !done; {
		select {
		case <-replaced:
			done = true
		default:
		}
		var placement bytes.Buffer
		relay.writePlacement(&placement, 1, "")
		controls, body, ok := strings.Cut(placement.String(), ";")
		if ok && strings.Contains(controls, "a=T") && strings.TrimSuffix(body, "\x1b\\") == "" {
			bodyless++
		}
	}

	if got, want := bodyless, 0; got != want {
		t.Fatalf("transmissions carrying no body = %d, want %d", got, want)
	}
}

// The keys the relay owns are written after the ones it was handed, so a
// fragment assembled from another process's command cannot take over the
// action, the image id, the placement id, the cursor policy or the reply
// level.
func TestKittyRelayWritesItsOwnKeysLast(t *testing.T) {
	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)
	relay := vx.NewKittyRelay()
	mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")

	var placement bytes.Buffer
	relay.writePlacement(&placement, 7, "c=2,r=1,C=0,i=99,q=0")

	want := fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,c=2,r=1,C=0,i=99,q=0,a=T,i=%d,p=7,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
	if got := placement.String(); got != want {
		t.Fatalf("transmission = %q, want %q", got, want)
	}
}

// Once the terminal holds a generation, placing it again asks for the copy it
// has rather than sending the bytes a second time.
func TestKittyRelayReplacesWithoutRetransmitting(t *testing.T) {
	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)
	relay := vx.NewKittyRelay()
	mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")

	var first, second bytes.Buffer
	relay.writePlacement(&first, 7, "c=2,r=1")
	relay.writePlacement(&second, 7, "c=2,r=1")

	if got := first.String(); !strings.Contains(got, "a=T") {
		t.Fatalf("first placement = %q, want a transmission", got)
	}
	want := fmt.Sprintf("\x1b_Ga=p,i=%d,p=7,C=1,q=2\x1b\\", relay.ID())
	if got := second.String(); got != want {
		t.Fatalf("second placement = %q, want %q", got, want)
	}
}

// A body too large for one escape code is chunked: the first chunk carries the
// controls and m=1, every later chunk carries only m= and q=, and m=0 ends the
// transfer.
func TestKittyRelayChunksLargeBodies(t *testing.T) {
	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)
	relay := vx.NewKittyRelay()
	mustSetTransmit(
		t, relay,
		"f=32,s=8,v=8,t=d",
		strings.Repeat("A", kittyRelayChunk)+strings.Repeat("B", kittyRelayChunk)+"CC",
	)

	var placement bytes.Buffer
	relay.writePlacement(&placement, 3, "")

	tests := []struct {
		name    string
		keys    string
		payload string
	}{
		{
			name:    "first",
			keys:    fmt.Sprintf("f=32,s=8,v=8,t=d,a=T,i=%d,p=3,C=1,q=2,m=1", relay.ID()),
			payload: strings.Repeat("A", kittyRelayChunk),
		},
		{
			name:    "middle",
			keys:    "m=1,q=2",
			payload: strings.Repeat("B", kittyRelayChunk),
		},
		{
			name:    "last",
			keys:    "m=0,q=2",
			payload: "CC",
		},
	}

	escapes := strings.SplitAfter(placement.String(), "\x1b\\")
	if len(escapes) != len(tests)+1 || escapes[len(tests)] != "" {
		t.Fatalf("escape count = %d, want %d", len(escapes), len(tests)+1)
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			esc := strings.TrimSuffix(strings.TrimPrefix(escapes[i], "\x1b_G"), "\x1b\\")
			keys, payload, ok := strings.Cut(esc, ";")
			if !ok {
				t.Fatalf("chunk = %q, want keys and a payload", esc)
			}
			if keys != test.keys {
				t.Fatalf("chunk keys = %q, want %q", keys, test.keys)
			}
			if payload != test.payload {
				t.Fatalf("chunk payload length = %d, want %d", len(payload), len(test.payload))
			}
		})
	}
}

func retainedBytes(r *KittyRelay) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.payload)
}

// A relay keeps its body so that a later frame can send it again, but those
// bodies were produced by another process. Past the budget the oldest retained
// body is reclaimed, and an image whose body is gone places nothing at all
// rather than an empty image.
func TestKittyRelayReclaimsRetainedBodiesPastBudget(t *testing.T) {
	// Sized by hand against the 8 MiB ceiling: two of these bodies fit under
	// it, three do not.
	const body = 3 << 20

	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)
	oldest := vx.NewKittyRelay()
	mustSetTransmit(t, oldest, "f=32,s=8,v=8,t=d", strings.Repeat("A", body))
	middle := vx.NewKittyRelay()
	mustSetTransmit(t, middle, "f=32,s=8,v=8,t=d", strings.Repeat("B", body))

	if got := retainedBytes(oldest); got != body {
		t.Fatalf("retained body under the budget = %d bytes, want %d", got, body)
	}

	newest := vx.NewKittyRelay()
	mustSetTransmit(t, newest, "f=32,s=8,v=8,t=d", strings.Repeat("C", body))

	if got := retainedBytes(oldest); got != 0 {
		t.Fatalf("oldest retained body = %d bytes, want 0", got)
	}
	if got := retainedBytes(middle); got != body {
		t.Fatalf("middle retained body = %d bytes, want %d", got, body)
	}
	if got := retainedBytes(newest); got != body {
		t.Fatalf("newest retained body = %d bytes, want %d", got, body)
	}

	var placement bytes.Buffer
	oldest.writePlacement(&placement, 1, "")
	if got := placement.String(); got != "" {
		t.Fatalf("placement of a reclaimed image = %q, want no output", got)
	}
}

// The ceiling belongs to a [Vaxis], so what one terminal's relays retain
// cannot reclaim another terminal's bodies.
func TestKittyRelayBudgetIsPerVaxis(t *testing.T) {
	// Two of these exceed the 8 MiB ceiling, so a shared one would reclaim the
	// first of them.
	const body = 5 << 20

	var first, second bytes.Buffer
	one := newWriterTestVaxis(&first).NewKittyRelay()
	mustSetTransmit(t, one, "f=32,s=8,v=8,t=d", strings.Repeat("A", body))
	other := newWriterTestVaxis(&second).NewKittyRelay()
	mustSetTransmit(t, other, "f=32,s=8,v=8,t=d", strings.Repeat("B", body))

	if got := retainedBytes(one); got != body {
		t.Fatalf("first terminal retained body = %d bytes, want %d", got, body)
	}
	if got := retainedBytes(other); got != body {
		t.Fatalf("second terminal retained body = %d bytes, want %d", got, body)
	}
}

// Destroy frees the image without waiting for a render. An image the terminal
// was never given is not deleted at all: it has no id there to free.
func TestKittyRelayDestroyFreesTransmittedImagesOnly(t *testing.T) {
	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)

	untransmitted := vx.NewKittyRelay()
	mustSetTransmit(t, untransmitted, "f=32,s=8,v=8,t=d", "AAAA")
	untransmitted.Destroy()
	if got := out.String(); got != "" {
		t.Fatalf("destroying an untransmitted image wrote %q, want no output", got)
	}

	relay := vx.NewKittyRelay()
	mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")
	var placement bytes.Buffer
	relay.writePlacement(&placement, 4, "")

	out.Reset()
	relay.Destroy()
	want := fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", relay.ID())
	if got := out.String(); got != want {
		t.Fatalf("destroy wrote %q, want %q", got, want)
	}
}

// A frame that no longer places the image takes the placement off the screen
// and leaves the image itself alone.
func TestKittyRelayDeletesPlacementDroppedFromAFrame(t *testing.T) {
	var out bytes.Buffer
	vx := newWriterTestVaxis(&out)
	win := vx.Window()
	relay := vx.NewKittyRelay()

	mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")
	vx.graphicsNext = nil
	mustPlace(t, relay, win, 6, 2, 1, "")
	vx.render()
	if _, err := vx.tw.Flush(); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	vx.graphicsNext = nil
	vx.render()
	if _, err := vx.tw.Flush(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	want := fmt.Sprintf("\x1b_Ga=d,d=i,i=%d,p=6,q=2\x1b\\", relay.ID())
	if !strings.Contains(got, want) {
		t.Fatalf("dropped placement = %q, want %q", got, want)
	}
	if strings.Contains(got, "d=I") {
		t.Fatalf("dropped placement = %q, want the image itself kept", got)
	}
}

// The control string is spliced into an escape code the relay is building, so
// a ';' in it would end the payload early and an ESC would close the escape
// and let whatever follows open another. Both are refused, the image keeps the
// bytes it had, and the relay goes on working.
func TestKittyRelayRejectsControlsOutsideTheKeyAlphabet(t *testing.T) {
	tests := []struct {
		name     string
		controls string
	}{
		{name: "semicolon", controls: "f=32;a=T"},
		{name: "escape", controls: "f=32,\x1b_Ga=T"},
		{name: "space", controls: "f=32, s=8"},
		{name: "string terminator", controls: "f=32\x1b\\"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			vx := newWriterTestVaxis(&out)
			relay := vx.NewKittyRelay()

			if err := relay.SetTransmit(test.controls, "AAAA"); err == nil {
				t.Fatalf("SetTransmit(%q, ...) = nil, want an error", test.controls)
			}

			var refused bytes.Buffer
			relay.writePlacement(&refused, 1, "")
			if got := refused.String(); got != "" {
				t.Fatalf("placement after refused controls = %q, want no output", got)
			}

			mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")
			var accepted bytes.Buffer
			relay.writePlacement(&accepted, 1, "")
			want := fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,a=T,i=%d,p=1,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
			if got := accepted.String(); got != want {
				t.Fatalf("placement after a valid call = %q, want %q", got, want)
			}
		})
	}
}

// The body is written after the ';' that separates it from the keys, so a
// second ';' or an ESC in it escapes the payload the same way. The relay never
// decodes the body, which is exactly why it has to check the alphabet.
func TestKittyRelayRejectsPayloadOutsideBase64(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "semicolon", payload: "AA;A"},
		{name: "escape", payload: "AA\x1b\\"},
		{name: "newline", payload: "AA\nAA"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			vx := newWriterTestVaxis(&out)
			relay := vx.NewKittyRelay()

			if err := relay.SetTransmit("f=32,s=8,v=8,t=d", test.payload); err == nil {
				t.Fatalf("SetTransmit(..., %q) = nil, want an error", test.payload)
			}

			var refused bytes.Buffer
			relay.writePlacement(&refused, 1, "")
			if got := refused.String(); got != "" {
				t.Fatalf("placement after a refused payload = %q, want no output", got)
			}

			mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")
			var accepted bytes.Buffer
			relay.writePlacement(&accepted, 1, "")
			want := fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,a=T,i=%d,p=1,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
			if got := accepted.String(); got != want {
				t.Fatalf("placement after a valid call = %q, want %q", got, want)
			}
		})
	}
}

// A body the relay holds but has not written yet is exempt from the
// retained-body budget, because an image with no bytes has nothing to draw.
// The per-body ceiling is what keeps that exemption finite, and the control
// string has a cap of its own for the same reason.
func TestKittyRelayRejectsOversizedFragments(t *testing.T) {
	tests := []struct {
		name     string
		controls string
		payload  string
	}{
		{
			name:     "body over the ceiling",
			controls: "f=32,s=8,v=8,t=d",
			payload:  strings.Repeat("A", kittyMaxPayloadBytes+1),
		},
		{
			name:     "controls over the cap",
			controls: strings.Repeat("a=1,", kittyMaxControlBytes/4) + "a=1",
			payload:  "AAAA",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			vx := newWriterTestVaxis(&out)
			relay := vx.NewKittyRelay()

			if err := relay.SetTransmit(test.controls, test.payload); err == nil {
				t.Fatal("SetTransmit = nil, want an error")
			}

			var refused bytes.Buffer
			relay.writePlacement(&refused, 1, "")
			// Counted rather than quoted: an oversized body that was not
			// refused is the whole of it, and printing that helps nobody.
			if got := refused.Len(); got != 0 {
				t.Fatalf("placement after a refused fragment = %d bytes, want 0", got)
			}
			if got := retainedBytes(relay); got != 0 {
				t.Fatalf("body retained for a refused fragment = %d bytes, want 0", got)
			}

			mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")
			var accepted bytes.Buffer
			relay.writePlacement(&accepted, 1, "")
			want := fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,a=T,i=%d,p=1,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
			if got := accepted.String(); got != want {
				t.Fatalf("placement after a valid call = %q, want %q", got, want)
			}
		})
	}
}

// The placement keys are spliced in the same way the control string is, so
// they are held to the same alphabet. A refused call queues no placement at
// all, which is what keeps the frame it was refused in unchanged.
func TestKittyRelayRejectsPlacementKeysOutsideTheKeyAlphabet(t *testing.T) {
	tests := []struct {
		name string
		keys string
	}{
		{name: "semicolon", keys: "c=2;a=T"},
		{name: "escape", keys: "c=2,\x1b_Ga=T"},
		{name: "space", keys: "c=2, r=1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			vx := newWriterTestVaxis(&out)
			win := vx.Window()
			relay := vx.NewKittyRelay()
			mustSetTransmit(t, relay, "f=32,s=8,v=8,t=d", "AAAA")

			vx.graphicsNext = nil
			if err := relay.Place(win, 1, 2, 1, test.keys); err == nil {
				t.Fatalf("Place(..., %q) = nil, want an error", test.keys)
			}
			if got := len(vx.graphicsNext); got != 0 {
				t.Fatalf("placements queued for refused keys = %d, want 0", got)
			}
			vx.render()
			if _, err := vx.tw.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); strings.Contains(got, "\x1b_G") {
				t.Fatalf("frame after refused keys = %q, want no graphics escape", got)
			}

			out.Reset()
			vx.graphicsNext = nil
			mustPlace(t, relay, win, 1, 2, 1, "c=2,r=1")
			vx.render()
			if _, err := vx.tw.Flush(); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,c=2,r=1,a=T,i=%d,p=1,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
			if got := out.String(); !strings.Contains(got, want) {
				t.Fatalf("frame after valid keys = %q, want %q", got, want)
			}
		})
	}
}

// m= belongs to the relay the way the keys above it do. A fragment carrying
// m=1 would otherwise leave the terminal in an open chunked transfer, feeding
// it every image escape that follows, so an unchunked transmission ends its
// key list with the m=0 that closes the transfer.
func TestKittyRelayEndsUnchunkedTransfersItself(t *testing.T) {
	tests := []struct {
		name          string
		controls      string
		placementKeys string
	}{
		{name: "controls", controls: "f=32,s=8,v=8,t=d,m=1", placementKeys: "c=2,r=1"},
		{name: "placement keys", controls: "f=32,s=8,v=8,t=d", placementKeys: "c=2,r=1,m=1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			vx := newWriterTestVaxis(&out)
			relay := vx.NewKittyRelay()
			mustSetTransmit(t, relay, test.controls, "AAAA")

			var placement bytes.Buffer
			relay.writePlacement(&placement, 5, test.placementKeys)

			want := fmt.Sprintf(
				"\x1b_G%s,%s,a=T,i=%d,p=5,C=1,q=2,m=0;AAAA\x1b\\",
				test.controls, test.placementKeys, relay.ID(),
			)
			if got := placement.String(); got != want {
				t.Fatalf("transmission = %q, want %q", got, want)
			}
		})
	}
}
