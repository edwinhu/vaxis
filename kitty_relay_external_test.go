package vaxis_test

import (
	"fmt"
	"strings"
	"testing"

	"go.rockorager.dev/vaxis"
)

// The whole public story of a relayed image: reserve an id, hand the relay the
// bytes another process produced, place it in a window, move it, replace its
// bytes, and free it. The relay never decodes any of it, so what the test
// checks is what reaches the terminal.
func TestKittyRelayTransmitsPlacesAndDestroys(t *testing.T) {
	console := newPrimaryConsole(80, 10)
	vx, err := vaxis.New(vaxis.Options{
		DisableMouse: true,
		WithConsole:  console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vx.Close()

	relay := vx.NewKittyRelay()
	if relay.ID() == 0 {
		t.Fatal("relay was not given an image id")
	}

	// A first frame transmits the bytes and places them at the window's
	// origin, cursor untouched.
	vx.Window().Clear()
	if err := relay.SetTransmit("f=32,s=8,v=8,t=d", "AAAA"); err != nil {
		t.Fatal(err)
	}
	if err := relay.Place(vx.Window().New(2, 3, 10, 5), 9, 10, 5, "c=10,r=5"); err != nil {
		t.Fatal(err)
	}
	console.ResetOutput()
	vx.Render()

	want := fmt.Sprintf("\x1b[4;3H\x1b_Gf=32,s=8,v=8,t=d,c=10,r=5,a=T,i=%d,p=9,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
	if got := console.Output(); !strings.Contains(got, want) {
		t.Fatalf("first frame %q missing %q", got, want)
	}

	// Moving the image sends no bytes: the terminal still holds them, so the
	// old placement is removed and the copy it has is placed again.
	vx.Window().Clear()
	if err := relay.Place(vx.Window().New(20, 4, 10, 5), 9, 10, 5, "c=10,r=5"); err != nil {
		t.Fatal(err)
	}
	console.ResetOutput()
	vx.Render()

	got := console.Output()
	for _, want := range []string{
		fmt.Sprintf("\x1b_Ga=d,d=i,i=%d,p=9,q=2\x1b\\", relay.ID()),
		fmt.Sprintf("\x1b[5;21H\x1b_Ga=p,i=%d,p=9,C=1,q=2\x1b\\", relay.ID()),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("moved frame %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "a=T") {
		t.Fatalf("moved frame = %q, want no second transmission", got)
	}

	// New bytes for the same image replace it where it stands. Nothing is
	// deleted first, or the image would blank for a frame every time the
	// process behind it sends one.
	vx.Window().Clear()
	if err := relay.SetTransmit("f=32,s=8,v=8,t=d", "BBBB"); err != nil {
		t.Fatal(err)
	}
	if err := relay.Place(vx.Window().New(20, 4, 10, 5), 9, 10, 5, "c=10,r=5"); err != nil {
		t.Fatal(err)
	}
	console.ResetOutput()
	vx.Render()

	got = console.Output()
	want = fmt.Sprintf("\x1b_Gf=32,s=8,v=8,t=d,c=10,r=5,a=T,i=%d,p=9,C=1,q=2,m=0;BBBB\x1b\\", relay.ID())
	if !strings.Contains(got, want) {
		t.Fatalf("replaced frame %q missing %q", got, want)
	}
	if strings.Contains(got, "a=d") {
		t.Fatalf("replaced frame = %q, want no delete before the new bytes", got)
	}

	console.ResetOutput()
	relay.Destroy()

	got = console.Output()
	want = fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", relay.ID())
	if !strings.Contains(got, want) {
		t.Fatalf("destroy %q missing %q", got, want)
	}
}

// The fragments a relay is handed were assembled from a command another
// process wrote, so the public API is where they have to be refused. A control
// string that closes the escape code, or placement keys that open a second one
// behind it, would otherwise reach the terminal as commands of their own: the
// keys below end the relay's escape and ask the terminal to free every image
// it holds. Nothing of a refused call is written, and the relay still works
// afterwards.
func TestKittyRelayRejectsFragmentsItCannotSplice(t *testing.T) {
	console := newPrimaryConsole(80, 10)
	vx, err := vaxis.New(vaxis.Options{
		DisableMouse: true,
		WithConsole:  console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vx.Close()

	relay := vx.NewKittyRelay()
	win := vx.Window().New(2, 3, 10, 5)

	vx.Window().Clear()
	if err := relay.SetTransmit("f=32,s=8,v=8,t=d;AAAA\x1b\\\x1b_Ga=T,i=1", "AAAA"); err == nil {
		t.Fatal("SetTransmit with a control string that closes the escape = nil, want an error")
	}
	if err := relay.Place(win, 9, 10, 5, "c=10,r=5"); err != nil {
		t.Fatal(err)
	}
	console.ResetOutput()
	vx.Render()

	if got := console.Output(); strings.Contains(got, "\x1b_G") {
		t.Fatalf("frame after refused controls = %q, want no graphics escape", got)
	}

	vx.Window().Clear()
	if err := relay.SetTransmit("f=32,s=8,v=8,t=d", "AAAA"); err != nil {
		t.Fatal(err)
	}
	if err := relay.Place(win, 9, 10, 5, "c=10,r=5\x1b\\\x1b_Ga=d,d=A"); err == nil {
		t.Fatal("Place with keys that open a second escape = nil, want an error")
	}
	console.ResetOutput()
	vx.Render()

	if got := console.Output(); strings.Contains(got, "\x1b_G") {
		t.Fatalf("frame after refused placement keys = %q, want no graphics escape", got)
	}

	// The bytes and the keys the relay did accept go out as they would have
	// without the refusals.
	vx.Window().Clear()
	if err := relay.Place(win, 9, 10, 5, "c=10,r=5"); err != nil {
		t.Fatal(err)
	}
	console.ResetOutput()
	vx.Render()

	want := fmt.Sprintf("\x1b[4;3H\x1b_Gf=32,s=8,v=8,t=d,c=10,r=5,a=T,i=%d,p=9,C=1,q=2,m=0;AAAA\x1b\\", relay.ID())
	if got := console.Output(); !strings.Contains(got, want) {
		t.Fatalf("frame after valid fragments %q missing %q", got, want)
	}
}

// Ids come from the one allocator, so a relayed image cannot collide with an
// image the application transmitted itself.
func TestReserveGraphicIDDoesNotRepeat(t *testing.T) {
	console := newPrimaryConsole(80, 10)
	vx, err := vaxis.New(vaxis.Options{
		DisableMouse: true,
		WithConsole:  console,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer vx.Close()

	seen := map[uint64]bool{}
	for i := 0; i < 4; i++ {
		for _, id := range []uint64{vx.ReserveGraphicID(), vx.NewKittyRelay().ID()} {
			if id == 0 {
				t.Fatal("reserved image id = 0, want a real id")
			}
			if seen[id] {
				t.Fatalf("image id %d was handed out twice", id)
			}
			seen[id] = true
		}
	}
}
