package term

// Round-2 gates.
//
// TestKittyPassthroughPayloadReleasedAfterWrite is the TOTAL RETAINED-BYTES
// variant, not the release-after-write variant. The release-after-write form is
// not expressible here: the bytes are pinned by (*vaxis.KittyRelay).payload, and
// the only thing that clears it after a write is the render loop, which this
// package cannot drive -- *vaxis.Vaxis has no exported constructor that takes a
// writer. What IS expressible, and is the same defect measured a different way,
// is that nothing bounds the sum of the payloads live relays hold: a child that
// keeps several images open pins every one of their base64 bodies forever. The
// gate asserts a ceiling on that sum.

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.rockorager.dev/vaxis"
)

// kittyMaxPlacementsPerImage is the cap round 2 must enforce on the placements
// one image may hold at once.
const wantKittyMaxPlacementsPerImage = 256

// kittyMaxRetainedPayloadBytes is the ceiling round 2 must enforce on the sum of
// the base64 bodies live relays pin in memory.
const wantKittyMaxRetainedPayloadBytes = 8 << 20

// A child that re-places one image under a fresh p= every frame -- which is what
// a browser painting a scrolling page does -- must not grow the widget's
// placement list without bound.
func TestKittyPassthroughPlacementCap(t *testing.T) {
	host := &fakeKittyHost{kitty: true}
	vt, _ := newPassthroughModel(t, host)

	vt.WriteString("\x1b_Ga=T,f=32,s=800,v=480,t=d,i=7,p=1,q=2;AAAA\x1b\\")

	const placements = 5000
	for n := 2; n <= placements; n += 1 {
		vt.WriteString(fmt.Sprintf("\x1b_Ga=p,i=7,p=%d,q=2\x1b\\", n))
	}

	s := vt.kitty
	if s == nil {
		t.Fatal("no kitty state after transmit")
	}
	ki := s.images[uint64(7)]
	if ki == nil {
		t.Fatal("no image for the child's i=7")
	}
	if got := len(ki.places); got > wantKittyMaxPlacementsPerImage {
		t.Fatalf("placements grew to %d, want <= %d", got, wantKittyMaxPlacementsPerImage)
	}
	if got := len(vt.graphics); got > wantKittyMaxPlacementsPerImage {
		t.Fatalf("graphics grew to %d, want <= %d", got, wantKittyMaxPlacementsPerImage)
	}
}

// The relay pins the child's base64 body so a later frame can be re-transmitted.
// Nothing bounds the total, so a handful of open images holds tens of megabytes
// of base64 that no render will ever read again.
func TestKittyPassthroughPayloadReleasedAfterWrite(t *testing.T) {
	vx := &vaxis.Vaxis{}
	body := strings.Repeat("A", 1<<20)

	relays := make([]*vaxis.KittyRelay, 0, 16)
	for i := 0; i < 16; i += 1 {
		r := vx.NewKittyRelay()
		// Checked, not discarded: a refused body is retained by nobody, so a
		// silent refusal here would leave the ceiling below satisfied by an
		// empty budget rather than by a bounded one.
		if err := r.SetTransmit("f=32,s=800,v=480,t=d", body); err != nil {
			t.Fatalf("SetTransmit: %v", err)
		}
		relays = append(relays, r)
	}

	total := 0
	for _, r := range relays {
		total += retainedPayloadBytes(r)
	}
	if total > wantKittyMaxRetainedPayloadBytes {
		t.Fatalf("relays retain %d bytes of payload, want <= %d", total, wantKittyMaxRetainedPayloadBytes)
	}
}

// retainedPayloadBytes reads the relay's pinned body. reflect is used because
// the field is unexported in another package and there is no accessor; Len on a
// read-only string Value is allowed.
func retainedPayloadBytes(r *vaxis.KittyRelay) int {
	return reflect.ValueOf(r).Elem().FieldByName("payload").Len()
}
