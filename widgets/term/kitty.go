package term

import (
	"strconv"
	"strings"
)

// kittyCommand is one parsed kitty graphics escape code: the control key/value
// pairs plus the (still base64) payload.
//
// Nothing here holds pixels. The embedded terminal RELAYS a child's graphics to
// the host rather than decoding them, so a command is only ever taken apart far
// enough to decide policy and to rebuild a control string of our own.
type kittyCommand struct {
	action     byte // a=, default 't'
	medium     byte // t=, default 'd'
	format     int  // f=, default 32
	deleteWhat byte // d=, default 'a'

	width  int // s=, source pixel width for raw formats
	height int // v=, source pixel height for raw formats
	size   int // S=, bytes to read from a file/shm source
	offset int // O=, byte offset into a file/shm source

	id        uint32 // i=
	number    uint32 // I=
	placement uint32 // p=

	cols int // c=, display width in cells
	rows int // r=, display height in cells

	// x=, y=, w=, h= select the source rectangle: the region of the
	// transmitted image this placement draws.
	srcX int
	srcY int
	srcW int
	srcH int

	z int // z=, the placement's z-index

	// unicodePlaceholder is U=. A sender asking for unicode placeholders wants
	// the image positioned by placeholder CELLS it will print itself, which is
	// a layout this widget does not implement and must not silently ignore.
	unicodePlaceholder int

	quiet      int  // q=, 1 suppresses OK, 2 suppresses everything
	more       bool // m=1, another chunk follows
	compressed bool // o=z

	// moreSet records whether the sender named m= at all. Carrying m= is what
	// makes a command part of a chunked transfer, so a command without the key
	// is a whole command of its own and must never be folded into a transfer
	// left in flight.
	moreSet bool

	// C=. 1 means "do not move the cursor". cursorMovementSet records whether
	// the sender named C= at all; see moveCursorAfterKittyImage.
	cursorMovement    int
	cursorMovementSet bool

	payload string

	// base64OK is the result of scanning payload, and base64Checked says
	// whether that scan has been done. The scan is O(len) over bytes a child
	// chose, so it runs BEFORE the model lock is taken (prepareKittySequence)
	// and is carried here rather than repeated under the lock.
	base64OK      bool
	base64Checked bool

	// mediumName / mediumErr carry the result of validating a NAMED medium
	// (t=s/t=f/t=t), and mediumChecked says whether that validation has been
	// done. The validation is several syscalls against a path a child chose, so
	// like the base64 scan it runs BEFORE the model lock is taken
	// (Model.prepareKittySequence) and is carried here rather than repeated
	// under the lock.
	mediumName    string
	mediumErr     error
	mediumChecked bool
}

// kittyPrepared is the work a kitty APC needs that must NOT happen while the
// model lock is held.
type kittyPrepared struct {
	data     string
	payload  string
	base64OK bool

	// mediumName carries a named medium's validated name (t=s/t=f/t=t) and
	// mediumErr the refusal, so the syscalls that decide it run off the lock.
	// mediumChecked distinguishes "validated" from "not prepared".
	mediumName    string
	mediumErr     error
	mediumChecked bool
}

// prepareKittySequence does a kitty APC's per-frame work off the lock: the
// O(len) base64 scan of an inline payload, and the filesystem validation of a
// named source.
//
// A child can send megabytes of base64 per frame, and a named source costs an
// EvalSymlinks plus an Lstat; doing either inside Model.update, which holds
// vt.mu, stalls every Draw and every reader for that long. nil means there was
// nothing to prepare.
//
// It reads only vt.EnableKittyFileMedia, which is fixed at construction, so the
// receiver is safe to touch without the lock.
func (vt *Model) prepareKittySequence(data string) *kittyPrepared {
	cmd, ok := parseKittyCommand(data)
	if !ok {
		return nil
	}
	switch cmd.medium {
	case 'd':
		if cmd.payload == "" {
			return nil
		}
		return &kittyPrepared{
			data:     data,
			payload:  cmd.payload,
			base64OK: isKittyBase64(cmd.payload),
		}
	case 's', 'f', 't':
		if !kittyActionReadsMedium(cmd) {
			return nil
		}
		name, err := vt.validateKittyMedium(cmd)
		return &kittyPrepared{
			data:          data,
			payload:       cmd.payload,
			mediumName:    name,
			mediumErr:     err,
			mediumChecked: true,
		}
	default:
		return nil
	}
}

// kittyActionReadsMedium reports whether the locked path would ever consult a
// NAMED medium (t=s/t=f/t=t) for this command.
//
// Only a=q (kittyQuery) and a=t/a=T (kittyForwardPayload) reach
// validateKittyMedium. a=d frees, a=p places something already transmitted, and
// a=f/a=a/a=c are refused outright -- none of them has a source to read, and a
// U=1 placeholder command is refused before the action is dispatched at all.
// Validating on the medium alone would hand a child a filesystem probe on every
// one of those.
func kittyActionReadsMedium(cmd kittyCommand) bool {
	if cmd.unicodePlaceholder == 1 {
		return false
	}
	switch cmd.action {
	case 'q', 't', 'T':
		return true
	default:
		return false
	}
}

// kittyChunkBase64OK answers the scan for one command, preferring the result
// computed before the lock.
func kittyChunkBase64OK(cmd kittyCommand) bool {
	if cmd.base64Checked {
		return cmd.base64OK
	}
	return isKittyBase64(cmd.payload)
}

// parseKittyCommand parses the data vaxis's ANSI parser hands us for an APC
// sequence: the raw control string with the leading 'G' retained and no ESC_ /
// ESC\ framing, "G<k=v,...>[;<base64 payload>]".
//
// ok is false when the data is not a kitty graphics command at all, so the
// caller can fall back to posting an EventAPC. Unknown control keys are ignored
// rather than fatal, per the protocol's forward-compatibility rule.
func parseKittyCommand(data string) (kittyCommand, bool) {
	if !strings.HasPrefix(data, "G") {
		return kittyCommand{}, false
	}
	rest := data[1:]

	controls := rest
	payload := ""
	if i := strings.IndexByte(rest, ';'); i >= 0 {
		controls = rest[:i]
		payload = rest[i+1:]
	}

	cmd := kittyCommand{
		action:     't',
		medium:     'd',
		format:     32,
		deleteWhat: 'a',
		payload:    payload,
	}

	for _, pair := range strings.Split(controls, ",") {
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq != 1 || len(pair) < 3 {
			// Not key=value with a single-letter key: this is not a kitty
			// control string.
			return kittyCommand{}, false
		}
		key := pair[0]
		value := pair[2:]
		if !isASCIILetter(key) {
			return kittyCommand{}, false
		}

		switch key {
		case 'a':
			cmd.action = value[0]
		case 't':
			cmd.medium = value[0]
		case 'd':
			cmd.deleteWhat = value[0]
		case 'o':
			cmd.compressed = value == "z"
		case 'f':
			cmd.format = kittyInt(value, cmd.format)
		case 's':
			cmd.width = kittyInt(value, cmd.width)
		case 'v':
			cmd.height = kittyInt(value, cmd.height)
		case 'S':
			cmd.size = kittyInt(value, cmd.size)
		case 'O':
			cmd.offset = kittyInt(value, cmd.offset)
		case 'c':
			cmd.cols = kittyInt(value, cmd.cols)
		case 'r':
			cmd.rows = kittyInt(value, cmd.rows)
		case 'x':
			cmd.srcX = kittyInt(value, cmd.srcX)
		case 'y':
			cmd.srcY = kittyInt(value, cmd.srcY)
		case 'w':
			cmd.srcW = kittyInt(value, cmd.srcW)
		case 'h':
			cmd.srcH = kittyInt(value, cmd.srcH)
		case 'z':
			cmd.z = kittyInt(value, cmd.z)
		case 'U':
			cmd.unicodePlaceholder = kittyInt(value, cmd.unicodePlaceholder)
		case 'q':
			cmd.quiet = kittyInt(value, cmd.quiet)
		case 'C':
			cmd.cursorMovement = kittyInt(value, cmd.cursorMovement)
			cmd.cursorMovementSet = true
		case 'm':
			cmd.more = kittyInt(value, 0) == 1
			cmd.moreSet = true
		case 'i':
			cmd.id = kittyUint32(value, cmd.id)
		case 'I':
			cmd.number = kittyUint32(value, cmd.number)
		case 'p':
			cmd.placement = kittyUint32(value, cmd.placement)
		default:
			// Unknown key: ignore it. A future protocol revision adding keys
			// must not make existing images undisplayable.
		}
	}

	return cmd, true
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func kittyInt(value string, fallback int) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func kittyUint32(value string, fallback uint32) uint32 {
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return fallback
	}
	return uint32(n)
}

// kittyMaxChunkedBytes bounds the base64 text accumulated for one transfer. The
// control data is attacker-controlled, so a sender that never ends its transfer
// would otherwise grow this without limit.
const kittyMaxChunkedBytes = 32 << 20

// kittyAssembly is a chunked transfer in flight.
type kittyAssembly struct {
	// cmd holds the FIRST chunk's controls, which are authoritative for the
	// whole transfer; its payload field is empty and unused.
	cmd kittyCommand
	// payload accumulates the base64 text. Non-final chunks are guaranteed to
	// be a multiple of 4 bytes, so concatenating the encoded text is safe.
	payload strings.Builder
	// base64OK is the AND of every chunk's scan. A concatenation of base64
	// chunks is base64 exactly when each chunk is, so the whole transfer is
	// judged without ever rescanning the accumulated body.
	base64OK bool
}

func (a *kittyAssembly) append(payload string) bool {
	if len(payload) > kittyMaxChunkedBytes-a.payload.Len() {
		return false
	}
	a.payload.WriteString(payload)
	return true
}

// assembleKittyChunks folds a chunked transmission back into a single command.
//
// complete is false when the sequence was a non-final chunk, or when a
// malformed transfer was dropped: in both cases there is nothing to act on yet.
func (vt *Model) assembleKittyChunks(cmd kittyCommand) (kittyCommand, bool) {
	s := vt.kitty
	if s == nil || s.inflight == nil {
		if !cmd.more {
			// The common case: a whole image in one sequence. The byte
			// ceiling has to bound this path as well as the accumulator, or
			// a sender simply declines to chunk and opts out of it.
			if len(cmd.payload) > kittyMaxChunkedBytes {
				vt.replyKitty(cmd, errKitty("EINVAL:payload too large"))
				return kittyCommand{}, false
			}
			cmd.base64OK, cmd.base64Checked = kittyChunkBase64OK(cmd), true
			return cmd, true
		}
		s = vt.kittyStateOf()
		first := cmd
		first.payload = ""
		in := &kittyAssembly{cmd: first, base64OK: kittyChunkBase64OK(cmd)}
		if in.append(cmd.payload) {
			s.inflight = in
		}
		return kittyCommand{}, false
	}

	in := s.inflight
	if !isKittyContinuation(in.cmd, cmd) {
		// A sender must finish one image before sending any other graphics
		// command. Something else arrived, so the partial transfer is
		// abandoned rather than spliced into an unrelated command.
		s.inflight = nil
		return vt.assembleKittyChunks(cmd)
	}

	if !in.append(cmd.payload) {
		s.inflight = nil
		return kittyCommand{}, false
	}
	in.base64OK = in.base64OK && kittyChunkBase64OK(cmd)
	if cmd.more {
		return kittyCommand{}, false
	}

	done := in.cmd
	done.payload = in.payload.String()
	done.base64OK, done.base64Checked = in.base64OK, true
	done.more = false
	s.inflight = nil
	return done, true
}

// isKittyContinuation reports whether cmd continues the transfer first opened.
//
// The m= key is the whole test: the protocol says continuation chunks carry
// only m and optionally q, so a command WITHOUT m= is a complete command of its
// own, never a continuation. Treating an m-less command as one means a single
// lost final chunk swallows every image that follows it, forever.
func isKittyContinuation(first, cmd kittyCommand) bool {
	if !cmd.moreSet {
		return false
	}
	if cmd.id != 0 && cmd.id != first.id {
		return false
	}
	if cmd.number != 0 && cmd.number != first.number {
		return false
	}
	// 't' is the default for a=, so an omitted action parses as 't'.
	return cmd.action == 't' || cmd.action == first.action
}

// clampKittyCellGeometry bounds a placement's footprint to the screen.
//
// c= and r= are the display size in CELLS and are taken straight off the wire,
// so a mail body rendered through this widget can name two billion rows. The
// natural footprint has the same problem from the other end: a source at the
// pixel ceiling over a one-pixel cell measures in the hundreds of thousands of
// rows, and moveCursorAfterKittyImage runs one ind() per row.
func (vt *Model) clampKittyCellGeometry(img *Image) {
	if w := vt.width(); w > 0 && img.cols > w {
		img.cols = w
	}
	if h := vt.height(); h > 0 && img.rows > h {
		img.rows = h
	}
	img.cols = max(1, img.cols)
	img.rows = max(1, img.rows)
}

// moveCursorAfterKittyImage moves the cursor past the image, but only when the
// sender explicitly asked for that with C=0.
//
// The protocol's own default is C=0, i.e. move. Inside an embedded terminal
// that default reads as "shove the page down on every image", so movement is
// opt-in here: a sender that wants it says so.
func (vt *Model) moveCursorAfterKittyImage(img *Image, cmd kittyCommand) {
	if !cmd.cursorMovementSet || cmd.cursorMovement != 0 {
		return
	}
	// ind() rather than a bare row increment, so the scroll region and the
	// scrollback are honoured.
	startCol := vt.cursor.col
	for i := 0; i < img.rows; i += 1 {
		vt.ind()
	}
	vt.cursor.col = min(startCol+column(img.cols), column(vt.width()-1))
	vt.lastCol = false
}

// replyKitty answers the sender. The protocol requires a response to a=q, and
// to any command carrying a non-zero image id or number; q=1 suppresses the OK
// responses and q=2 suppresses failures as well.
//
// Getting this exactly right is what makes the relay usable: a sender probes
// with a=q and no q= key at all, so it MUST hear back, while the frames it then
// sends carry q=2 and must hear nothing.
func (vt *Model) replyKitty(cmd kittyCommand, err error) {
	if cmd.action != 'q' && cmd.id == 0 && cmd.number == 0 {
		return
	}
	if err == nil && cmd.quiet >= 1 {
		return
	}
	if err != nil && cmd.quiet >= 2 {
		return
	}

	var b strings.Builder
	b.WriteString("\x1B_Gi=")
	b.WriteString(strconv.FormatUint(uint64(cmd.id), 10))
	if cmd.number != 0 {
		b.WriteString(",I=")
		b.WriteString(strconv.FormatUint(uint64(cmd.number), 10))
	}
	if cmd.placement != 0 {
		b.WriteString(",p=")
		b.WriteString(strconv.FormatUint(uint64(cmd.placement), 10))
	}
	b.WriteByte(';')
	if err == nil {
		b.WriteString("OK")
	} else {
		b.WriteString(sanitizeKittyReply(err.Error()))
	}
	b.WriteString("\x1B\\")

	vt.enqueueReplyString(b.String())
}

// sanitizeKittyReply keeps an error message from terminating or extending the
// APC we are writing back into the child's input stream.
func sanitizeKittyReply(msg string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return ' '
		}
		return r
	}, msg)
}
