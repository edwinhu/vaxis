package term

import (
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"go.rockorager.dev/vaxis"
)

// stageKittyPerfFile writes a regular file under a staging directory whose
// basename kittyStagedName accepts, and returns its path.
func stageKittyPerfFile(t *testing.T) string {
	t.Helper()
	// t=f and t=s validation is only offered for a local host terminal.
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "px-kittyperf.rgba")
	if err := os.WriteFile(path, []byte("\x00\x00\x00\xff"), 0o600); err != nil {
		t.Fatalf("stage source: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve staged source: %v", err)
	}
	if !isKittyInStagingDir(resolved) {
		t.Skipf("t.TempDir() %q is not a kitty staging directory on this host", resolved)
	}
	return path
}

// kittyPerfNamedFrame is a transmit-and-display naming an external source.
func kittyPerfNamedFrame(medium byte, name string) string {
	return "\x1b_Ga=T,f=32,s=800,v=480,t=" + string(medium) +
		",i=9,p=1,C=1,q=2;" + base64.StdEncoding.EncodeToString([]byte(name)) + "\x1b\\"
}

// (A) The syscalls that validate a NAMED medium (t=f / t=s) must happen in the
// off-lock prepare step, not under vt.mu with every Draw and reader waiting.
//
// The observable is the stat itself: kittyLstat records whether vt.mu was held
// when it ran. RED today, because validateKittyMedium is reached from
// applySequence, which runs under the lock.
func TestKittyPerfNamedMediaPreparedOffLock(t *testing.T) {
	path := stageKittyPerfFile(t)

	for _, medium := range []byte{'f', 's'} {
		t.Run(string(medium), func(t *testing.T) {
			name := path
			if medium == 's' {
				// A t=s name is a flat shm object name, and its file is
				// looked for under /dev/shm; skip unless it can be staged.
				name = "/" + filepath.Base(path)
				shm := filepath.Join(kittyShmDir, filepath.Base(path))
				if err := os.WriteFile(shm, []byte("\x00\x00\x00\xff"), 0o600); err != nil {
					t.Skipf("cannot stage a shm object: %v", err)
				}
				t.Cleanup(func() { _ = os.Remove(shm) })
			}

			host := &fakeKittyHost{kitty: true}
			vt, _ := newPassthroughModel(t, host)
			vt.EnableKittyFileMedia = true

			var mu sync.Mutex
			var stats int
			var underLock bool
			restore := kittyLstat
			kittyLstat = func(p string) (fs.FileInfo, error) {
				held := true
				if vt.mu.TryLock() {
					vt.mu.Unlock()
					held = false
				}
				mu.Lock()
				stats++
				underLock = underLock || held
				mu.Unlock()
				return restore(p)
			}
			t.Cleanup(func() { kittyLstat = restore })

			vt.WriteString(kittyPerfNamedFrame(medium, name))

			mu.Lock()
			gotStats, gotUnderLock := stats, underLock
			mu.Unlock()

			if gotStats == 0 {
				t.Fatalf("t=%s source was never stat'd; validation did not run", string(medium))
			}
			if gotUnderLock {
				t.Fatalf("t=%s source validated UNDER vt.mu (%d stats); want it prepared off the lock",
					string(medium), gotStats)
			}
		})
	}
}

// (B) kittyStagingDirs resolves symlinks for every root on every containment
// check. The set is process-wide and effectively fixed, so it must be built
// once: the EvalSymlinks work for N checks must equal the work for one.
func TestKittyPerfStagingDirsCached(t *testing.T) {
	var mu sync.Mutex
	var calls int
	restore := kittyEvalSymlinks
	kittyEvalSymlinks = func(p string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return restore(p)
	}
	t.Cleanup(func() { kittyEvalSymlinks = restore })

	count := func(n int) int {
		resetKittyStagingDirs()
		mu.Lock()
		calls = 0
		mu.Unlock()
		for i := 0; i < n; i++ {
			isKittyInStagingDir("/tmp/px-kittyperf.rgba")
		}
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	one := count(1)
	if one == 0 {
		t.Fatalf("EvalSymlinks calls for one check = 0, want the staging roots to be resolved at least once")
	}
	five := count(5)
	if five != one {
		t.Fatalf("EvalSymlinks calls: 5 checks = %d, 1 check = %d; want the staging dirs built once and reused",
			five, one)
	}
}

// perfLockRelay records whether vt.mu was held when the host write in Destroy
// ran. Destroy is a synchronous write to the real terminal, so holding the
// model lock across it blocks every Draw for the length of that write.
type perfLockRelay struct {
	id        uint64
	vt        *Model
	destroyed int
	underLock bool
}

func (r *perfLockRelay) ID() uint64 { return r.id }

func (r *perfLockRelay) SetTransmit(controls, payload string) error {
	return nil
}

func (r *perfLockRelay) Place(win vaxis.Window, pid uint32, cols, rows int, placementKeys string) error {
	return nil
}

func (r *perfLockRelay) Destroy() {
	r.destroyed++
	if r.vt.mu.TryLock() {
		r.vt.mu.Unlock()
		return
	}
	r.underLock = true
}

type perfLockHost struct {
	kitty  bool
	vt     *Model
	relays []*perfLockRelay
}

func (h *perfLockHost) CanKittyGraphics() bool { return h.kitty }

func (h *perfLockHost) NewKittyRelay() kittyRelay {
	relay := &perfLockRelay{id: uint64(5001 + len(h.relays)), vt: h.vt}
	h.relays = append(h.relays, relay)
	return relay
}

// (C) The a=d delete path frees the host image while still holding vt.mu, so
// the host-tty write in Destroy runs under the lock. releaseKittyRelays already
// shows the shape of the fix: collect under the lock, destroy after unlocking.
// The image must still be freed exactly once.
func TestKittyPerfDestroyOffLock(t *testing.T) {
	host := &perfLockHost{kitty: true}
	vt, r := newReplyTestModel(t)
	host.vt = vt
	vt.kittyHost = host
	vt.resize(80, 24)
	vt.size = vaxis.Resize{Cols: 80, Rows: 24, XPixel: 800, YPixel: 480}

	vt.WriteString(passthroughFrame)
	vt.Draw(vaxis.Window{Width: 80, Height: 24})
	vt.WriteString(passthroughDelete7)
	assertNoReply(t, r)

	if len(host.relays) != 1 {
		t.Fatalf("relays = %d, want 1", len(host.relays))
	}
	relay := host.relays[0]
	if relay.destroyed != 1 {
		t.Fatalf("destroyed = %d, want 1", relay.destroyed)
	}
	if relay.underLock {
		t.Fatalf("relay.Destroy() ran UNDER vt.mu; want the host write done after the lock is released")
	}
}

// (D) The destroy drain is deferred from Model.update, which runs once per
// parsed rune or escape. It takes vt.mu unconditionally, before it can see
// that nothing is queued, so ordinary text doubles the lock acquisitions on
// the hottest path. Nothing pending must mean nothing locked.
func TestKittyPerfDrainNoLockWhenIdle(t *testing.T) {
	host := &fakeKittyHost{kitty: true}
	vt, _ := newPassthroughModel(t, host)

	// A fresh model: no a=d has been driven, so pendingDestroy is empty.
	if s := vt.kitty; s != nil && len(s.pendingDestroy) != 0 {
		t.Fatalf("pendingDestroy = %d, want an idle drain queue", len(s.pendingDestroy))
	}

	resetKittyDrainLocks()
	vt.WriteString("hello world\r\n")
	vt.WriteString("plain text, no kitty, no APC\r\n")

	if got := atomic.LoadInt32(&kittyDrainLocks); got != 0 {
		t.Fatalf("idle drain took vt.mu %d time(s) for non-kitty input; want 0 (guard the drain with a lock-free check)", got)
	}
}
