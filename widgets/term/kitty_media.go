package term

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
)

// WithKittyFileMedia controls whether a child's kitty graphics command may name
// a file or a POSIX shared-memory object as the source of its pixels (t=f, t=t,
// t=s) rather than carrying them inline.
//
// It is off by default. Those media name a path taken directly from the child's
// escape stream, and this terminal RELAYS that name to the HOST terminal, which
// then opens it -- so enabling them for a child that renders untrusted input
// hands the host an attacker-chosen path to read.
func WithKittyFileMedia(enabled bool) Option {
	return func(m *Model) {
		m.EnableKittyFileMedia = enabled
	}
}

const (
	// kittyShmDir is where Linux keeps POSIX shared-memory objects: shm_open
	// is implemented as open() against this tmpfs.
	kittyShmDir = "/dev/shm"

	// kittyTempMarker is the substring the protocol requires in the name of a
	// t=t source.
	kittyTempMarker = "tty-graphics-protocol"
)

// kittyStagingDirs are the only directories a legitimate sender stages pixels
// in. Reads are confined to them by allowlist rather than by naming forbidden
// trees: naming the forbidden places leaves everything unnamed -- $HOME, the
// mail store, the git checkout -- reachable by any child that can write an
// escape sequence, and aerc runs an untrusted mail body through a filter on
// this widget.
// kittyEvalSymlinks and kittyLstat are the two syscall-backed steps of source
// validation, named so a test can count and time them.
var (
	kittyEvalSymlinks = filepath.EvalSymlinks
	kittyLstat        = os.Lstat
)

// kittyStagingCache memoizes the staging roots. Deriving them costs one
// EvalSymlinks per root, and isKittyInStagingDir runs on every validated frame
// -- the roots are process-stable in a real terminal, so the work is done once.
var kittyStagingCache atomic.Pointer[[]string]

// resetKittyStagingDirs drops whatever kittyStagingDirs has memoized.
func resetKittyStagingDirs() { kittyStagingCache.Store(nil) }

func kittyStagingDirs() []string {
	if cached := kittyStagingCache.Load(); cached != nil {
		return *cached
	}
	dirs := buildKittyStagingDirs()
	kittyStagingCache.Store(&dirs)
	return dirs
}

func buildKittyStagingDirs() []string {
	named := []string{"/tmp", kittyShmDir, os.TempDir()}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		named = append(named, tmp)
	}
	// Carry the symlink-resolved form of each root as well: a source path is
	// resolved before it is checked, so a staging root reached through a
	// symlink would otherwise never match its own contents.
	dirs := make([]string, 0, 2*len(named))
	for _, dir := range named {
		dirs = append(dirs, dir)
		if resolved, err := kittyEvalSymlinks(dir); err == nil && resolved != dir {
			dirs = append(dirs, resolved)
		}
	}
	return dirs
}

// isKittyInStagingDir reports whether an already-resolved path lies inside one
// of those directories.
func isKittyInStagingDir(resolved string) bool {
	for _, dir := range kittyStagingDirs() {
		if isKittyWithin(dir, resolved) {
			return true
		}
	}
	return false
}

// isKittyWithin reports whether path lies strictly inside dir, comparing whole
// path elements so that /tmpfoo does not count as being under /tmp.
func isKittyWithin(dir string, path string) bool {
	dir = filepath.Clean(dir)
	if dir == "/" {
		return true
	}
	return strings.HasPrefix(filepath.Clean(path), dir+string(filepath.Separator))
}

// kittyStagedName reports whether a basename is one a graphics sender
// deliberately staged.
//
// Containment in a staging directory alone is not enough: a temp directory is
// shared with every other program on the machine -- editor swap files,
// downloaded attachments, another session's scratch tree -- so "somewhere under
// /tmp" still names plenty worth handing to the host. Three names qualify: the
// protocol's own tty-graphics-protocol marker, terminal-browser's frame files,
// and the shared-memory objects it stages frames in.
func kittyStagedName(base string) bool {
	switch {
	case strings.Contains(base, kittyTempMarker):
		return true
	case strings.HasPrefix(base, "terminal-browser-") && strings.HasSuffix(base, ".rgba"):
		return true
	case strings.HasPrefix(base, "px-"):
		return true
	}
	return false
}

// resolveKittyPath follows symlinks, as the protocol requires, and reports the
// symlink loop or missing file as an error rather than naming something else.
func resolveKittyPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", errKitty("EINVAL:invalid path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", errKitty("ENOENT:cannot resolve source")
	}
	return resolved, nil
}

// validateKittyFile checks a t=f / t=t source and returns the RESOLVED path.
//
// The resolved path is what gets forwarded to the host, not the child's
// original: forwarding the original re-opens the TOCTOU window the resolution
// closed, because the symlink the child named may point somewhere else by the
// time the host follows it.
func validateKittyFile(path string) (string, error) {
	resolved, err := resolveKittyPath(path)
	if err != nil {
		return "", err
	}
	if !isKittyInStagingDir(resolved) {
		return "", errKitty("EPERM:source is not in a graphics staging directory")
	}
	if !kittyStagedName(filepath.Base(resolved)) {
		return "", errKitty("EPERM:source is not named as a staged graphics file")
	}
	if err := checkKittyRegularAndOwned(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// validateKittyShm checks a t=s source and returns the shared-memory NAME.
//
// The name is a flat namespace with a conventional leading '/', so anything
// left looking like a path after that slash is a traversal attempt: without
// this check "/../../etc/shadow" resolves out of the tmpfs entirely.
func validateKittyShm(name string) (string, error) {
	trimmed := strings.TrimPrefix(name, "/")
	if trimmed == "" || trimmed == "." || trimmed == ".." ||
		strings.ContainsRune(trimmed, '/') || strings.ContainsRune(trimmed, 0) {
		return "", errKitty("EINVAL:invalid shared memory object name")
	}
	if !kittyStagedName(trimmed) {
		return "", errKitty("EPERM:shared memory object is not named as a staged frame")
	}
	if err := checkKittyRegularAndOwned(filepath.Join(kittyShmDir, trimmed)); err != nil {
		return "", err
	}
	return name, nil
}

// checkKittyRegularAndOwned refuses anything that is not an ordinary file this
// user owns. Device files, FIFOs and sockets are not images, and a file owned
// by somebody else was not staged by the child we are relaying for.
func checkKittyRegularAndOwned(path string) error {
	fi, err := kittyLstat(path)
	if err != nil {
		return errKitty("ENOENT:cannot stat source")
	}
	if !fi.Mode().IsRegular() {
		return errKitty("EPERM:only regular files may be used as a source")
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errKitty("EPERM:cannot establish source ownership")
	}
	if int(stat.Uid) != os.Getuid() {
		return errKitty("EPERM:source is not owned by this user")
	}
	return nil
}

// kittyHostIsLocal reports whether the host terminal is on this machine.
//
// Over SSH the host opens the name in ITS OWN filesystem, so a path validated
// here names a different file there -- at best nothing, at worst somebody
// else's. Relaying file and shm media is only meaningful locally.
func kittyHostIsLocal() bool {
	return os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_TTY") == ""
}

// errKitty builds a reply error. The text before the colon is the code the
// protocol puts on the wire, so it must stay stable.
func errKitty(msg string) error {
	return errors.New(msg)
}

func errKittyf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
