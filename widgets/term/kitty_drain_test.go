package term

// Paint-on-drain: the widget must paint the instant the pty has nothing more to
// say, and keep the 8 ms timer only as a coalescing cap for bytes that are still
// arriving. Both tests drive the real StartWithSize path -- a real pty, a real
// child, the real parser goroutine -- and observe vaxis.Redraw{} through the
// real event handler, because the thing under test IS that goroutine's
// scheduling decision and nothing smaller contains it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.rockorager.dev/vaxis"
)

func (s redrawSource) name() string {
	switch s {
	case redrawSourceNone:
		return "redrawSourceNone"
	case redrawSourceTimer:
		return "redrawSourceTimer"
	case redrawSourceDrain:
		return "redrawSourceDrain"
	}
	return "redrawSource(?)"
}

// kittyDrainChild writes body to a file and returns a command that cats it to
// the pty once and then stays alive and completely silent. The silence is the
// point: the parser must see exactly these bytes and then a drained pty, which
// is the condition the drain path keys on. A child that EXITED instead would
// send ansi.EOF, and the goroutine returns on EOF without ever reaching the
// redraw branches.
func kittyDrainChild(t *testing.T, body string) *exec.Cmd {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frames")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write child frames: %v", err)
	}
	return exec.Command("sh", "-c", "cat '"+path+"'; exec sleep 30")
}

// kittyDrainModel starts a passthrough-capable 80x24 model on a real pty whose
// child emits body, and returns a channel carrying every vaxis.Redraw{} the
// widget dispatches. paintOnDrain selects which scheduling decision the model
// is under test for.
func kittyDrainModel(t *testing.T, body string, paintOnDrain bool) (*Model, <-chan vaxis.Redraw) {
	t.Helper()
	vt := New()
	vt.kittyHost = &fakeKittyHost{kitty: true}
	vt.PaintOnDrain = paintOnDrain
	// Pixel geometry, so a frame sized in pixels has a cell geometry to be
	// placed at. resize() does not touch vt.size, and this runs before the
	// parser goroutine exists.
	vt.size = vaxis.Resize{Cols: 80, Rows: 24, XPixel: 800, YPixel: 480}

	// New() arms the timer with time.NewTimer(0), so one fire is already queued
	// before StartWithSize installs the goroutine. That Redraw belongs to no
	// frame and would be indistinguishable from one; drain it here.
	if !vt.timer.Stop() {
		<-vt.timer.C
	}

	redraws := make(chan vaxis.Redraw, 4096)
	vt.Attach(func(ev vaxis.Event) {
		if r, ok := ev.(vaxis.Redraw); ok {
			select {
			case redraws <- r:
			default:
			}
		}
	})

	if err := vt.StartWithSize(kittyDrainChild(t, body), 80, 24); err != nil {
		t.Fatalf("StartWithSize: %v", err)
	}
	t.Cleanup(vt.Close)
	return vt, redraws
}

// One frame, then a quiet and drained pty: the frame the user is waiting on must
// be painted immediately, not after the 8 ms coalescing timer. The break this
// catches is the timer being applied to a frame there is nothing left to
// coalesce it with.
func TestKittyDrainSingleFramePaintsImmediately(t *testing.T) {
	vt, redraws := kittyDrainModel(t, passthroughFrame, true)

	start := time.Now()
	select {
	case <-redraws:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("no vaxis.Redraw{} within 500ms of the child's single frame")
	}
	observed := time.Since(start)

	if got := vt.lastRedrawSource(); got != redrawSourceDrain {
		t.Fatalf("lastRedrawSource() = %s after one frame on a drained pty, want %s",
			got.name(), redrawSourceDrain.name())
	}

	// Secondary, and deliberately not a gate: the source flag above decides this
	// test. Wall clock here is an upper bound only -- start is taken after
	// StartWithSize returns, so it may already include part of the child's own
	// startup.
	t.Logf("secondary (not asserted): frame -> Redraw observed in %v; redraws()=%d", observed, vt.redraws())
}

// The same single frame on the same drained pty, with the flag OFF: the widget
// must go back to the 8 ms coalescing timer and paint from there. The break this
// catches is the drain path running for every caller regardless of the flag --
// which is exactly what it does today, so this test is RED until the goroutine
// consults PaintOnDrain.
func TestKittyDrainOffKeepsTimer(t *testing.T) {
	vt, redraws := kittyDrainModel(t, passthroughFrame, false)

	select {
	case <-redraws:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("no vaxis.Redraw{} within 500ms of the child's single frame")
	}

	if got := vt.lastRedrawSource(); got != redrawSourceTimer {
		t.Fatalf("lastRedrawSource() = %s after one frame with PaintOnDrain off, want %s",
			got.name(), redrawSourceTimer.name())
	}
	// The timer path owes exactly one paint for one frame; a second would mean
	// the drain path also fired for it.
	if got := vt.redraws(); got != 1 {
		t.Fatalf("redraws() = %d after one frame with PaintOnDrain off, want 1", got)
	}
}

// A dense burst -- every frame buffered ahead of the parser in a single write --
// must still collapse into a handful of paints. This is the regression guard on
// the drain path: a drain check that fires per parsed sequence would turn one
// scroll into hundreds of full-pane repaints. It must hold both before and after
// paint-on-drain lands.
func TestKittyDrainBurstStillCoalesces(t *testing.T) {
	const frames = 240
	vt, redraws := kittyDrainModel(t, strings.Repeat(passthroughFrame, frames), true)

	// Count every Redraw until the widget has been quiet for 150ms.
	const quiet = 150 * time.Millisecond
	settle := time.NewTimer(quiet)
	defer settle.Stop()
	deadline := time.After(5 * time.Second)
	var count int
loop:
	for {
		select {
		case <-redraws:
			count++
			settle.Reset(quiet)
		case <-settle.C:
			break loop
		case <-deadline:
			t.Logf("deadline reached with %d Redraws; the widget never went quiet", count)
			break loop
		}
	}

	if count == 0 {
		t.Fatalf("a %d-frame burst produced no vaxis.Redraw{} at all", frames)
	}
	if want := frames / 4; count >= want {
		t.Fatalf("a %d-frame burst produced %d Redraws, want fewer than %d: the burst is no longer coalesced",
			frames, count, want)
	}
	t.Logf("%d frames -> %d Redraws (redraws()=%d, last source %s)",
		frames, count, vt.redraws(), vt.lastRedrawSource().name())
}
