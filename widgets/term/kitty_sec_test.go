package term

import (
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// kittySecNamedControls is a kitty control string in the form the ANSI parser
// hands prepareKittySequence: leading 'G', no ESC_ / ESC\ framing, the name
// carried base64 in the payload.
func kittySecNamedControls(action byte, medium byte, name string) string {
	return "Ga=" + string(action) + ",f=32,s=800,v=480,t=" + string(medium) +
		",i=9,p=1,C=1,q=2;" + base64.StdEncoding.EncodeToString([]byte(name))
}

// countKittySecLstats runs fn with kittyLstat counting its calls, restoring the
// original afterwards, and reports how many stats the filesystem saw.
func countKittySecLstats(t *testing.T, fn func()) int {
	t.Helper()

	var mu sync.Mutex
	var stats int
	restore := kittyLstat
	kittyLstat = func(p string) (fs.FileInfo, error) {
		mu.Lock()
		stats++
		mu.Unlock()
		return restore(p)
	}
	defer func() { kittyLstat = restore }()

	fn()

	mu.Lock()
	defer mu.Unlock()
	return stats
}

// A command that is not a transmit or a query has no source to read: a=d frees
// an image, a=p places one already transmitted, a=c clears, and U=1 asks for a
// placement layout this widget refuses. Validating on the MEDIUM alone makes
// every one of them resolve and stat a child-chosen path for nothing -- an
// attacker-chosen filesystem probe on a code path that never wanted the bytes.
//
// The control case keeps the gate honest: a=T / a=q must still be validated, so
// a fix that simply stops validating cannot pass this test.
func TestKittySecPrepareSkipsNonTransmit(t *testing.T) {
	// t=f validation is only offered for a local host terminal.
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "px-kittysec.rgba")
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

	host := &fakeKittyHost{kitty: true}
	vt, _ := newPassthroughModel(t, host)
	vt.EnableKittyFileMedia = true

	for _, action := range []byte{'d', 'p'} {
		t.Run("nontransmit_a="+string(action), func(t *testing.T) {
			data := kittySecNamedControls(action, 'f', path)
			stats := countKittySecLstats(t, func() {
				vt.prepareKittySequence(data)
			})
			if stats != 0 {
				t.Fatalf("a=%s,t=f stat'd the source %d time(s); want 0 -- prepare must not probe the filesystem for a non-transmit action",
					string(action), stats)
			}
		})
	}

	for _, action := range []byte{'T', 'q'} {
		t.Run("control_a="+string(action), func(t *testing.T) {
			data := kittySecNamedControls(action, 'f', path)
			var prepared *kittyPrepared
			stats := countKittySecLstats(t, func() {
				prepared = vt.prepareKittySequence(data)
			})
			if stats == 0 {
				t.Fatalf("a=%s,t=f never stat'd the source; want validation to still run for a transmit/query",
					string(action))
			}
			if prepared == nil || !prepared.mediumChecked {
				t.Fatalf("a=%s,t=f prepared = %+v; want mediumChecked with the validated name carried",
					string(action), prepared)
			}
			if prepared.mediumErr != nil {
				t.Fatalf("a=%s,t=f mediumErr = %v; want the staged source accepted", string(action), prepared.mediumErr)
			}
			if prepared.mediumName != resolved {
				t.Fatalf("a=%s,t=f mediumName = %q, want %q", string(action), prepared.mediumName, resolved)
			}
		})
	}
}
