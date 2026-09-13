package term

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"

	"go.rockorager.dev/vaxis"
	"go.rockorager.dev/vaxis/log"
)

// kittyDrainLocks counts how many times drainKittyDestroys has taken vt.mu.
// It observes the lock traffic on the sequence path; the tests read it.
var kittyDrainLocks int32

func resetKittyDrainLocks() { atomic.StoreInt32(&kittyDrainLocks, 0) }

// kittyRelayHost is the host terminal as the embedded terminal sees it.
type kittyRelayHost interface {
	CanKittyGraphics() bool
	NewKittyRelay() kittyRelay
}

// kittyRelay is one host-side image whose bytes come from the child.
//
// SetTransmit and Place refuse a fragment they cannot splice into an escape
// code -- one carrying a byte outside the alphabet its kind is made of, or one
// longer than the relay is willing to write. A refusal changes nothing: the
// image keeps the bytes it already had and no placement is queued, so the
// caller's only move is to drop the command it was relaying.
type kittyRelay interface {
	ID() uint64
	SetTransmit(controls, payload string) error
	Place(win vaxis.Window, pid uint32, cols, rows int, placementKeys string) error
	Destroy()
}

// kittyFileFrameRelay is the extra a relay may offer: taking a raw pixel frame
// straight from the file the child staged it in, so the bytes never become an
// escape code at all. It is deliberately NOT part of kittyRelay -- a relay
// without it simply keeps the base64 tty path.
type kittyFileFrameRelay interface {
	SetTransmitFrameFile(path string, width, height int, format string) bool
}

// vaxisKittyHost adapts the real *vaxis.Vaxis to the interface above.
type vaxisKittyHost struct {
	vx *vaxis.Vaxis
}

func (h vaxisKittyHost) CanKittyGraphics() bool { return h.vx.CanKittyGraphics() }

func (h vaxisKittyHost) NewKittyRelay() kittyRelay { return h.vx.NewKittyRelay() }

// kittyMaxImages bounds the images one child may hold open on the HOST at once.
// A sender using a fresh i= per frame -- the ordinary convention, and what a
// browser child does -- would otherwise leak a host image per frame.
const kittyMaxImages = 32

// kittyMaxPlacementsPerImage bounds the placements one image may hold at once.
//
// Every a=p under a fresh p= allocates an *Image in vt.graphics and an entry in
// the image's own list, and p= is the CHILD's to choose: a sender that re-places
// one image under a new placement id each frame -- which is what a browser
// painting a scrolling page does -- would otherwise grow both lists for as long
// as it runs. Past the cap the oldest placement of that image is retired, which
// also tells the host to drop it.
const kittyMaxPlacementsPerImage = 256

// kittyAnonPIDBase is where synthetic placement ids start. The kitty protocol
// reserves nothing, but a sender that omits p= still needs a stable placement
// id on the host, and keeping ours far from the small numbers senders pick
// makes a collision inside one image's namespace implausible.
const kittyAnonPIDBase uint32 = 0x40000000

// kittyImage is one image the child transmitted, mapped onto one host image.
type kittyImage struct {
	key     uint64
	id      uint32 // child i=
	number  uint32 // child I=
	relay   kittyRelay
	places  []*Image
	deleted bool
}

// kittyPlacement is the kitty half of an *Image in vt.graphics. The *Image
// itself carries no pixels (img.img stays nil): the host holds them.
type kittyPlacement struct {
	image   *kittyImage
	pid     uint32 // child p=
	hostPID uint32
	keys    string // rebuilt c=, r=, x=, y=, w=, h=, z=
}

// kittyState is the per-Model relay state. It lives on Model rather than in a
// package-global map so two terminals cannot see each other's images.
type kittyState struct {
	inflight *kittyAssembly
	images   map[uint64]*kittyImage
	order    []uint64 // oldest first, for the kittyMaxImages cap
	nextPID  uint32

	// pendingDestroy holds relays whose host image has been freed in the model
	// but not yet on the host. Destroy is a synchronous write to the real
	// terminal, so it is collected here under vt.mu and performed after the
	// lock is released; see drainKittyDestroys.
	pendingDestroy []kittyRelay
}

func (vt *Model) kittyStateOf() *kittyState {
	if vt.kitty == nil {
		vt.kitty = &kittyState{images: map[uint64]*kittyImage{}}
	}
	return vt.kitty
}

// kittyHostTerminal is the host, with a fallback to the vaxis Draw attached.
// WithVaxis installs the adapter up front; this covers a Model built without
// that option whose first Draw supplied one.
func (vt *Model) kittyHostTerminal() kittyRelayHost {
	if vt.kittyHost != nil {
		return vt.kittyHost
	}
	if vt.vx != nil {
		vt.kittyHost = vaxisKittyHost{vx: vt.vx}
		return vt.kittyHost
	}
	return nil
}

func (vt *Model) hostCanKittyGraphics() bool {
	host := vt.kittyHostTerminal()
	return host != nil && host.CanKittyGraphics()
}

// kittyGraphics handles an APC as a kitty graphics command. It reports whether
// the sequence was one; false means the caller posts EventAPC as before.
func (vt *Model) kittyGraphics(data string) bool {
	cmd, ok := parseKittyCommand(data)
	if !ok {
		return false
	}
	if p := vt.kittyPrep; p != nil && p.data == data {
		cmd.base64OK, cmd.base64Checked = p.base64OK, true
		// A prepared medium result belongs to the exact command it was computed
		// from. A chunked transfer's controls are the FIRST chunk's while its
		// name arrives in pieces, so a command carrying m= at all keeps none of
		// it and the locked path validates the assembled command live.
		if p.mediumChecked && !cmd.moreSet {
			cmd.mediumName, cmd.mediumErr, cmd.mediumChecked = p.mediumName, p.mediumErr, true
		}
	}
	cmd, complete := vt.assembleKittyChunks(cmd)
	if complete {
		vt.applyKittyCommand(cmd)
	}
	// An incomplete command was a chunk of a larger transfer. It was still a
	// kitty command, so it is consumed rather than leaked to the application.
	return true
}

func (vt *Model) applyKittyCommand(cmd kittyCommand) {
	if cmd.unicodePlaceholder == 1 {
		// U=1 asks us to position the image by placeholder cells the sender
		// prints itself. Consuming that in silence leaves a sender waiting
		// for a picture that will never be drawn where it expects it.
		vt.replyKitty(cmd, errKitty("ENOTSUP:unicode placeholders are not supported"))
		return
	}
	switch cmd.action {
	case 'q':
		vt.replyKitty(cmd, vt.kittyQuery(cmd))
	case 'T', 't':
		vt.kittyTransmit(cmd)
	case 'p':
		vt.kittyRePlace(cmd)
	case 'd':
		vt.kittyDelete(cmd)
	default:
		// The animation actions (a=f, a=a, a=c) are not relayed. They are
		// still kitty commands, so they are consumed rather than leaked to
		// the application -- but a sender that asked for a response must hear
		// one, or it waits forever instead of falling back.
		vt.replyKitty(cmd, errKittyf("ENOTSUP:action a=%s not supported", string(cmd.action)))
	}
}

// kittyQuery answers a=q locally. A sender probes before it will use the
// protocol at all, so an unanswered query reads as "no kitty support"; a probe
// answered OK for a medium we would later refuse is worse, because the sender
// then paints with a transport that silently drops every frame.
func (vt *Model) kittyQuery(cmd kittyCommand) error {
	if !vt.hostCanKittyGraphics() {
		return errKitty("ENOTSUP:host terminal has no kitty graphics support")
	}
	switch cmd.medium {
	case 'd':
		return nil
	case 'f', 't', 's':
		_, err := vt.kittyMediumResult(cmd)
		return err
	default:
		return errKittyf("EINVAL:transmission medium %q not supported", string(cmd.medium))
	}
}

// kittyMediumResult is validateKittyMedium's answer for this command, taken
// from the off-lock prepare step when it has one.
//
// The prepared answer is only ever attached to the command it was computed from
// (kittyGraphics matches on the raw APC data), so this returns the same verdict
// the live call would -- it just does not pay EvalSymlinks and Lstat again while
// vt.mu is held. The live call remains for commands that reached here without a
// prepare: an assembled chunked transfer, or a caller that is not the APC path.
func (vt *Model) kittyMediumResult(cmd kittyCommand) (string, error) {
	if cmd.mediumChecked {
		return cmd.mediumName, cmd.mediumErr
	}
	return vt.validateKittyMedium(cmd)
}

// validateKittyMedium decides whether a non-inline source may be named to the
// HOST, and returns the name the host should be given.
func (vt *Model) validateKittyMedium(cmd kittyCommand) (string, error) {
	if !vt.EnableKittyFileMedia {
		return "", errKitty("EPERM:file and shared memory transmission media are disabled")
	}
	if !kittyHostIsLocal() {
		return "", errKitty("EPERM:file and shared memory media need a local host terminal")
	}
	decoded, err := decodeKittyName(cmd.payload)
	if err != nil {
		return "", err
	}
	switch cmd.medium {
	case 's':
		return validateKittyShm(decoded)
	case 'f', 't':
		return validateKittyFile(decoded)
	default:
		return "", errKittyf("EINVAL:transmission medium %q not supported", string(cmd.medium))
	}
}

// decodeKittyName decodes the base64 NAME a non-inline medium carries. Names
// are short, so there is no size concern here; the point is that a name is
// bytes, not text, until it has been decoded.
func decodeKittyName(payload string) (string, error) {
	enc := base64.StdEncoding
	if len(payload)%4 != 0 {
		enc = base64.RawStdEncoding
	}
	raw, err := enc.DecodeString(payload)
	if err != nil {
		return "", errKitty("EINVAL:bad base64 payload")
	}
	return string(raw), nil
}

// kittyForwardPayload is the payload to relay to the host.
//
// For t=d the child's own base64 is passed through, but only after it is
// confirmed to BE base64: an ESC or a backslash in it would terminate the APC
// we are writing to the host and let the child inject its own control
// sequences into the real terminal. For the named media the validated name is
// re-encoded, so the bytes on the wire are ours.
func (vt *Model) kittyForwardPayload(cmd kittyCommand) (string, error) {
	if cmd.medium == 'd' {
		if !kittyChunkBase64OK(cmd) {
			return "", errKitty("EINVAL:payload is not base64")
		}
		return cmd.payload, nil
	}
	name, err := vt.kittyMediumResult(cmd)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(name)), nil
}

func isKittyBase64(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '+', c == '/', c == '=', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// kittyForwardControls rebuilds the transmission control string from the PARSED
// command.
//
// The child's own control bytes are never forwarded. They are an unvalidated
// string from an untrusted process, and everything this terminal writes ends up
// inside an APC on the HOST's stream; rebuilding from typed fields is what makes
// an injection impossible rather than merely unlikely. a=, i= and p= are absent
// deliberately: the relay supplies its own.
func kittyForwardControls(cmd kittyCommand) string {
	parts := make([]string, 0, 7)
	parts = append(parts, fmt.Sprintf("f=%d", cmd.format))
	if cmd.width > 0 {
		parts = append(parts, fmt.Sprintf("s=%d", cmd.width))
	}
	if cmd.height > 0 {
		parts = append(parts, fmt.Sprintf("v=%d", cmd.height))
	}
	parts = append(parts, fmt.Sprintf("t=%s", string(cmd.medium)))
	if cmd.compressed {
		parts = append(parts, "o=z")
	}
	if cmd.size > 0 {
		parts = append(parts, fmt.Sprintf("S=%d", cmd.size))
	}
	if cmd.offset > 0 {
		parts = append(parts, fmt.Sprintf("O=%d", cmd.offset))
	}
	return strings.Join(parts, ",")
}

// kittyPlacementKeys rebuilds the placement half of the control string, again
// from typed fields only.
func kittyPlacementKeys(cmd kittyCommand) string {
	parts := make([]string, 0, 7)
	if cmd.cols > 0 {
		parts = append(parts, fmt.Sprintf("c=%d", cmd.cols))
	}
	if cmd.rows > 0 {
		parts = append(parts, fmt.Sprintf("r=%d", cmd.rows))
	}
	if cmd.srcX > 0 {
		parts = append(parts, fmt.Sprintf("x=%d", cmd.srcX))
	}
	if cmd.srcY > 0 {
		parts = append(parts, fmt.Sprintf("y=%d", cmd.srcY))
	}
	if cmd.srcW > 0 {
		parts = append(parts, fmt.Sprintf("w=%d", cmd.srcW))
	}
	if cmd.srcH > 0 {
		parts = append(parts, fmt.Sprintf("h=%d", cmd.srcH))
	}
	if cmd.z != 0 {
		parts = append(parts, fmt.Sprintf("z=%d", cmd.z))
	}
	return strings.Join(parts, ",")
}

// kittyKey is the child's name for an image. i= and I= are separate namespaces,
// so they are kept apart here too.
func kittyKey(cmd kittyCommand) (uint64, bool) {
	switch {
	case cmd.id != 0:
		return uint64(cmd.id), true
	case cmd.number != 0:
		return 1<<32 | uint64(cmd.number), true
	default:
		return 0, false
	}
}

func (vt *Model) kittyTransmit(cmd kittyCommand) {
	host := vt.kittyHostTerminal()
	if host == nil || !host.CanKittyGraphics() {
		vt.replyKitty(cmd, errKitty("ENOTSUP:host terminal has no kitty graphics support"))
		return
	}
	payload, err := vt.kittyForwardPayload(cmd)
	if err != nil {
		vt.replyKitty(cmd, err)
		return
	}

	ki := vt.kittyImageFor(cmd, host)
	if !vt.transmitKittyFrameFile(ki, cmd) {
		if err := ki.relay.SetTransmit(kittyForwardControls(cmd), payload); err != nil {
			// The relay would not take these bytes and kept the ones it
			// already had, so this command has no image behind it. Placing
			// anyway would show the PREVIOUS frame as though this one had
			// landed, and answering OK would leave the sender with no reason
			// to try again; the command is dropped and the sender told which
			// fragment was refused.
			vt.replyKitty(cmd, errKittyf("EINVAL:%s", err))
			return
		}
	}

	if cmd.action == 'T' {
		err = vt.placeKittyRelay(ki, cmd)
	}
	vt.replyKitty(cmd, err)
}

// transmitKittyFrameFile offers the frame to the relay's own graphics sink
// instead of the tty, and reports whether the sink took it. A false answer
// leaves the caller on the base64 path it has always used.
func (vt *Model) transmitKittyFrameFile(ki *kittyImage, cmd kittyCommand) bool {
	relay, ok := ki.relay.(kittyFileFrameRelay)
	if !ok {
		return false
	}
	path, format, ok := vt.kittyFrameFileSource(cmd)
	if !ok {
		return false
	}
	return relay.SetTransmitFrameFile(path, cmd.width, cmd.height, format)
}

// kittyFrameFileSource is the file a command's pixels are in, for the one shape
// a graphics sink can stream: a whole, uncompressed, raw rgba or rgb frame
// staged in a file or a shared-memory object.
//
// The path returned is the one validateKittyMedium RESOLVED, never the child's
// original, for the same reason the tty path forwards the resolved name: the
// symlink the child wrote may point somewhere else by the time it is opened.
// Everything else -- PNG data, inline bytes, a t=t temp file the host is
// supposed to unlink, a byte window carved out with O= or S= -- is left on the
// tty path, which handles all of it already.
func (vt *Model) kittyFrameFileSource(cmd kittyCommand) (path string, format string, ok bool) {
	if cmd.compressed {
		return "", "", false
	}
	var bpp int
	switch cmd.format {
	case 32:
		format, bpp = "rgba", 4
	case 24:
		format, bpp = "rgb", 3
	default:
		return "", "", false
	}
	// s= and v= come off the child's escape stream, so they are bounded here
	// rather than multiplied out first.
	if cmd.width <= 0 || cmd.height <= 0 ||
		cmd.width > kittyMaxFrameDimension || cmd.height > kittyMaxFrameDimension {
		return "", "", false
	}
	if cmd.offset != 0 || (cmd.size != 0 && cmd.size != cmd.width*cmd.height*bpp) {
		return "", "", false
	}

	name, err := vt.kittyMediumResult(cmd)
	if err != nil {
		return "", "", false
	}
	switch cmd.medium {
	case 'f':
		return name, format, true
	case 's':
		// A shared-memory name is a flat key in the tmpfs: validateKittyShm has
		// already refused anything carrying a separator, so joining it cannot
		// escape the directory.
		return filepath.Join(kittyShmDir, strings.TrimPrefix(name, "/")), format, true
	default:
		return "", "", false
	}
}

// kittyMaxFrameDimension bounds the pixel geometry a child may hand the
// graphics sink. Past it the frame stays on the tty path, where the host
// terminal applies its own limits.
const kittyMaxFrameDimension = 1 << 16

// kittyImageFor finds or creates the host image a command names. Reusing the
// entry is what keeps the HOST image id stable across frames, so the host
// replaces the picture in place instead of being handed a new image -- and a
// new image per frame is both a leak and a visible flash.
func (vt *Model) kittyImageFor(cmd kittyCommand, host kittyRelayHost) *kittyImage {
	s := vt.kittyStateOf()
	key, addressable := kittyKey(cmd)
	if addressable {
		if ki := s.images[key]; ki != nil && !ki.deleted {
			return ki
		}
	} else {
		// Nothing could name this image again, so it gets a private key that
		// no later command can collide with.
		s.nextPID += 1
		key = 1<<33 | uint64(s.nextPID)
	}

	ki := &kittyImage{
		key:    key,
		id:     cmd.id,
		number: cmd.number,
		relay:  host.NewKittyRelay(),
	}
	s.images[key] = ki
	s.order = append(s.order, key)
	vt.evictOldestKittyImages(s)
	return ki
}

// evictOldestKittyImages frees the host images beyond the cap, oldest first.
func (vt *Model) evictOldestKittyImages(s *kittyState) {
	for len(s.order) > kittyMaxImages {
		key := s.order[0]
		s.order = s.order[1:]
		if ki := s.images[key]; ki != nil {
			vt.destroyKittyImage(s, ki)
		}
	}
}

// placeKittyRelay records where the host should draw the image. It positions
// like the sixel path -- origin at the cursor, sourceRow scrollback-absolute --
// so every geometry path in term.go (scrollback shift, reflow, cull, clear)
// applies to a relayed image unchanged.
func (vt *Model) placeKittyRelay(ki *kittyImage, cmd kittyCommand) error {
	s := vt.kittyStateOf()

	hostPID := cmd.placement
	if hostPID == 0 {
		s.nextPID += 1
		hostPID = kittyAnonPIDBase | s.nextPID
	}

	cols := cmd.cols
	rows := cmd.rows
	if cols <= 0 {
		cols = vt.sixelCellCols(cmd.width)
	}
	if rows <= 0 {
		rows = vt.sixelCellRows(cmd.height)
	}

	// A repeated (image, placement) pair UPDATES the existing *Image rather
	// than allocating one. The host image id and the host placement id both
	// stay put, and the kitty protocol treats a re-transmission under the same
	// id as replacing the data, so the frame swaps atomically instead of
	// blanking between a delete and a place.
	for _, img := range ki.places {
		if img.kitty == nil || img.kitty.pid != cmd.placement {
			continue
		}
		img.cols, img.rows = cols, rows
		vt.clampKittyCellGeometry(img)
		img.origin.row = int(vt.cursor.row)
		img.origin.col = int(vt.cursor.col)
		img.sourceRow = vt.activeScreen.scrollbackLen() + int(vt.cursor.row)
		img.kitty.keys = kittyPlacementKeys(cmd)
		vt.moveCursorAfterKittyImage(img, cmd)
		return nil
	}

	if len(ki.places) >= kittyMaxPlacementsPerImage {
		vt.evictOldestKittyPlacements(ki)
		if len(ki.places) >= kittyMaxPlacementsPerImage {
			return errKitty("EINVAL:too many placements of one image")
		}
	}

	img := &Image{
		cols: cols,
		rows: rows,
		kitty: &kittyPlacement{
			image:   ki,
			pid:     cmd.placement,
			hostPID: hostPID,
			keys:    kittyPlacementKeys(cmd),
		},
	}
	vt.clampKittyCellGeometry(img)
	img.origin.row = int(vt.cursor.row)
	img.origin.col = int(vt.cursor.col)
	img.sourceRow = vt.activeScreen.scrollbackLen() + int(vt.cursor.row)

	vt.graphics = append(vt.graphics, img)
	ki.places = append(ki.places, img)
	vt.moveCursorAfterKittyImage(img, cmd)
	return nil
}

// evictOldestKittyPlacements retires placements, oldest first, until the image
// has room for one more.
func (vt *Model) evictOldestKittyPlacements(ki *kittyImage) {
	for len(ki.places) >= kittyMaxPlacementsPerImage {
		doomed := ki.places[0].kitty
		before := len(ki.places)
		vt.retireKittyPlacements(ki, func(kp *kittyPlacement) bool { return kp == doomed })
		if len(ki.places) >= before {
			// Nothing was retired, so retrying would spin. The caller
			// refuses the placement instead.
			return
		}
	}
}

// kittyRePlace implements a=p: display an image transmitted earlier. Answering
// ENOENT rather than dropping this silently is what tells a sender to
// re-transmit; a client that never hears back redraws forever.
func (vt *Model) kittyRePlace(cmd kittyCommand) {
	s := vt.kittyStateOf()
	key, addressable := kittyKey(cmd)
	if !addressable {
		vt.replyKitty(cmd, errKitty("ENOENT:no image with the given id"))
		return
	}
	ki := s.images[key]
	if ki == nil || ki.deleted {
		vt.replyKitty(cmd, errKitty("ENOENT:no image with the given id"))
		return
	}
	vt.replyKitty(cmd, vt.placeKittyRelay(ki, cmd))
}

// kittyDelete implements a=d. The d= key selects what goes; lowercase removes
// placements and keeps the host image so a later a=p still works, while
// uppercase frees the host image too.
func (vt *Model) kittyDelete(cmd kittyCommand) {
	s := vt.kitty
	if s == nil {
		return
	}
	what := cmd.deleteWhat
	freeData := what >= 'A' && what <= 'Z'
	if freeData {
		what += 'a' - 'A'
	}

	switch what {
	case 'a':
		for _, ki := range vt.kittyImagesSnapshot(s) {
			vt.retireKittyPlacements(ki, func(*kittyPlacement) bool { return true })
			if freeData {
				vt.destroyKittyImage(s, ki)
			}
		}
	case 'i':
		vt.deleteKittyByMatch(s, freeData, cmd, func(ki *kittyImage) bool {
			return ki.id != 0 && ki.id == cmd.id
		})
	case 'n':
		vt.deleteKittyByMatch(s, freeData, cmd, func(ki *kittyImage) bool {
			return ki.number != 0 && ki.number == cmd.number
		})
	case 'c':
		for _, ki := range vt.kittyImagesSnapshot(s) {
			vt.retireKittyPlacements(ki, func(kp *kittyPlacement) bool {
				return vt.kittyPlacementAtCursor(kp)
			})
		}
	default:
		// d=f deletes animation frames, which are not relayed, and the
		// remaining selectors address geometry this widget does not track.
		// Deleting nothing is safer than deleting the wrong image.
	}
}

func (vt *Model) deleteKittyByMatch(s *kittyState, freeData bool, cmd kittyCommand, match func(*kittyImage) bool) {
	for _, ki := range vt.kittyImagesSnapshot(s) {
		if !match(ki) {
			continue
		}
		vt.retireKittyPlacements(ki, func(kp *kittyPlacement) bool {
			// p= narrows the delete to a single placement.
			return cmd.placement == 0 || kp.pid == cmd.placement
		})
		if freeData {
			vt.destroyKittyImage(s, ki)
		}
	}
}

func (vt *Model) kittyImagesSnapshot(s *kittyState) []*kittyImage {
	out := make([]*kittyImage, 0, len(s.images))
	for _, key := range s.order {
		if ki := s.images[key]; ki != nil {
			out = append(out, ki)
		}
	}
	return out
}

func (vt *Model) kittyPlacementAtCursor(kp *kittyPlacement) bool {
	row, col := int(vt.cursor.row), int(vt.cursor.col)
	for _, img := range kp.image.places {
		if img.kitty != kp {
			continue
		}
		if row < img.origin.row || row >= img.origin.row+img.rows {
			return false
		}
		return col >= img.origin.col && col < img.origin.col+img.cols
	}
	return false
}

// retireKittyPlacements drops the matching placements out of the graphics list.
func (vt *Model) retireKittyPlacements(ki *kittyImage, match func(*kittyPlacement) bool) {
	doomed := map[*Image]struct{}{}
	for _, img := range ki.places {
		if img.kitty != nil && match(img.kitty) {
			doomed[img] = struct{}{}
		}
	}
	if len(doomed) == 0 {
		return
	}
	kept := vt.graphics[:0]
	for _, img := range vt.graphics {
		if _, ok := doomed[img]; ok {
			continue
		}
		kept = append(kept, img)
	}
	clearImageTail(vt.graphics, len(kept))
	vt.graphics = kept
	for img := range doomed {
		// destroy() retires the record from ki.places.
		img.destroy()
	}
}

// destroyKittyImage frees the host image and everything that referred to it.
//
// The host write itself is NOT done here. Every caller runs under vt.mu, and
// Destroy writes synchronously to the real terminal, so the relay is queued and
// drainKittyDestroys performs the write once the lock is released -- the shape
// releaseKittyRelays already uses. The ki.deleted guard above is what keeps
// each image queued, and so destroyed, exactly once.
func (vt *Model) destroyKittyImage(s *kittyState, ki *kittyImage) {
	if ki.deleted {
		return
	}
	vt.retireKittyPlacements(ki, func(*kittyPlacement) bool { return true })
	ki.deleted = true
	if ki.relay != nil {
		s.pendingDestroy = append(s.pendingDestroy, ki.relay)
		// Published under vt.mu; read lock-free by drainKittyDestroys.
		vt.kittyDestroyPending.Store(true)
	}
	delete(s.images, ki.key)
	for i, key := range s.order {
		if key == ki.key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// retireKittyImage is called from Image.destroy(), so an *Image dropped by any
// of term.go's own geometry paths -- a scrollback trim, a reflow, a clear --
// stops being placed without those paths knowing kitty exists.
func (kp *kittyPlacement) retire(img *Image) {
	ki := kp.image
	if ki == nil {
		return
	}
	kept := ki.places[:0]
	for _, place := range ki.places {
		if place == img {
			continue
		}
		kept = append(kept, place)
	}
	clearImageTail(ki.places, len(kept))
	ki.places = kept
}

// clearImageTail zeroes the slots a filter left beyond the new length.
// Reslicing shortens the view, not the backing array.
func clearImageTail(images []*Image, n int) {
	for i := n; i < len(images); i++ {
		images[i] = nil
	}
}

// drawKittyRelays re-places every visible relayed image at the widget's origin.
// It runs BEFORE drawGraphics, whose vaxis images are a different mechanism
// entirely; a relayed placement carries no pixels, so drawGraphics skips it.
func (vt *Model) drawKittyRelays(win vaxis.Window, graphics []positionedImage) {
	for _, graphic := range graphics {
		kp := graphic.img.kitty
		if kp == nil || kp.image == nil || kp.image.relay == nil || kp.image.deleted {
			continue
		}
		err := kp.image.relay.Place(
			win.New(graphic.col, graphic.row, -1, -1),
			kp.hostPID,
			graphic.img.cols,
			graphic.img.rows,
			kp.keys,
		)
		if err != nil {
			// A refused placement queued nothing, so this image is absent from
			// the frame and there is no half-drawn state to undo. There is
			// nobody to answer either: Draw runs on the application's
			// goroutine, long after the command these keys were rebuilt from
			// was acknowledged. The rest of the frame still has to be drawn.
			log.Error("kitty relay placement refused: %v", err)
		}
	}
}

// drainKittyDestroys performs the host writes destroyKittyImage queued. It is
// called from the sequence path AFTER vt.mu is released, so an a=d or an
// eviction frees the host image eagerly -- the render loop's placement delete
// does not reach a transmit-only or already-culled image -- without the write
// blocking Draw.
//
// The common case is that nothing is queued: update defers this call for every
// parsed rune, not just for an a=d. That case costs one atomic load and takes
// no lock at all. releaseKittyRelays stays the backstop for anything still
// queued when the terminal closes.
func (vt *Model) drainKittyDestroys() {
	if !vt.kittyDestroyPending.Load() {
		return
	}
	vt.mu.Lock()
	atomic.AddInt32(&kittyDrainLocks, 1)
	var pending []kittyRelay
	if s := vt.kitty; s != nil {
		pending, s.pendingDestroy = s.pendingDestroy, nil
	}
	vt.kittyDestroyPending.Store(false)
	vt.mu.Unlock()

	for _, relay := range pending {
		relay.Destroy()
	}
}

// releaseKittyRelays frees every host image this terminal opened. Close calls
// it after the pty is closed, so nothing can transmit a replacement in between
// and leave an image on the host that nothing will ever delete.
func (vt *Model) releaseKittyRelays() {
	vt.mu.Lock()
	s := vt.kitty
	vt.kitty = nil
	var relays []kittyRelay
	if s != nil {
		// Anything destroyKittyImage queued and nobody drained yet would
		// otherwise be dropped here, leaving an image on the host forever.
		relays = append(relays, s.pendingDestroy...)
		s.pendingDestroy = nil
		vt.kittyDestroyPending.Store(false)
		for _, ki := range vt.kittyImagesSnapshot(s) {
			vt.retireKittyPlacements(ki, func(*kittyPlacement) bool { return true })
			ki.deleted = true
			if ki.relay != nil {
				relays = append(relays, ki.relay)
			}
		}
	}
	vt.mu.Unlock()

	// Destroy writes to the host terminal, so it happens outside our lock.
	for _, relay := range relays {
		relay.Destroy()
	}
}
