package vaxis

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"go.rockorager.dev/vaxis/log"
	"go.rockorager.dev/vaxis/octreequant"
	"go.rockorager.dev/vaxis/sixel"
)

// Alpha value that we consider to be transparent enough to use default
// background color
const transparentEnough = 50

const (
	noGraphics = iota
	fullBlock
	halfBlock
	sixelGraphics
	kitty
)

func atomicLoad(val *int32) bool {
	return atomic.LoadInt32(val) == 1
}

func atomicStore(addr *int32, val bool) {
	if val {
		atomic.StoreInt32(addr, 1)
		return
	}
	atomic.StoreInt32(addr, 0)
}

// Image is a static image on the screen
type Image interface {
	// Draw draws the [Image] to the [Window]. The image will not be drawn
	// if it is larger than the window
	Draw(Window)
	// Destroy removes an image from memory. Call when done with this image
	Destroy()
	// Resizes the image to fit within the provided area. The image will not
	// be upscaled, nor will it's aspect ratio be changed
	Resize(w int, h int)
	// CellSize is the current cell size of the encoded image
	CellSize() (w int, h int)
}

// RemoveImage removes any active placements for img from future renders.
func (vx *Vaxis) RemoveImage(img Image) {
	id, ok := imageID(img)
	if !ok {
		return
	}
	vx.removeImagePlacement(id)
}

func imageID(img Image) (uint64, bool) {
	switch img := img.(type) {
	case *KittyImage:
		return img.id, true
	case *Sixel:
		return img.id, true
	case *FullBlockImage, *HalfBlockImage:
		return 0, false
	default:
		return 0, false
	}
}

func (vx *Vaxis) removeImagePlacement(id uint64) {
	if len(vx.graphicsNext) == 0 {
		return
	}
	next := make([]*placement, 0, len(vx.graphicsNext))
	for _, placement := range vx.graphicsNext {
		if placement.id != id {
			next = append(next, placement)
		}
	}
	vx.graphicsNext = next
}

// NewImage creates a new image using the highest quality renderer the terminal
// is capable of
func (vx *Vaxis) NewImage(img image.Image) (Image, error) {
	switch vx.graphicsProtocol {
	case fullBlock:
		return vx.NewFullBlockImage(img), nil
	case halfBlock:
		return vx.NewHalfBlockImage(img), nil
	case sixelGraphics:
		return vx.NewSixel(img), nil
	case kitty:
		return vx.NewKittyGraphic(img), nil
	default:
		return nil, fmt.Errorf("no supported image protocol")
	}
}

type KittyImage struct {
	vx       *Vaxis
	img      image.Image
	id       uint64
	w        int
	h        int
	reqW     int
	reqH     int
	cellPixW int
	cellPixH int
	uploaded int32
	encoding int32
	buf      *bytes.Buffer
}

func (vx *Vaxis) NewKittyGraphic(img image.Image) *KittyImage {
	log.Trace("new kitty image")
	k := &KittyImage{
		vx:  vx,
		img: img,
		id:  vx.nextGraphicID(),
		buf: bytes.NewBuffer(nil),
	}
	return k
}

// Draw draws the [Image] to the [Window].
func (k *KittyImage) Draw(win Window) {
	if atomicLoad(&k.encoding) {
		return
	}
	if k.reqW != 0 && k.reqH != 0 && k.cellSizeChanged() {
		k.Resize(k.reqW, k.reqH)
		return
	}
	col, row := win.Origin()
	log.Trace("placing kitty image at cell %d,%d", col, row)
	// the pid is a 32 bit number where the high 16bits are the width and
	// the low 16 are the height
	pid := uint(col)<<16 | uint(row)
	writeFunc := func(w io.Writer) {
		if !atomicLoad(&k.uploaded) {
			_, _ = w.Write(k.buf.Bytes())
			atomicStore(&k.uploaded, true)
			k.buf.Reset()
		}
		_, _ = fmt.Fprintf(w, "\x1B_Ga=p,i=%d,p=%d,C=1\x1B\\", k.id, pid)
	}
	deleteFunc := func(w io.Writer) {
		_, _ = fmt.Fprintf(w, "\x1B_Ga=d,d=i,i=%d,p=%d\x1B\\", k.id, pid)
	}
	placement := &placement{
		col:      col,
		row:      row,
		id:       k.id,
		w:        k.w,
		h:        k.h,
		writeTo:  writeFunc,
		deleteFn: deleteFunc,
	}
	k.vx.graphicsNext = append(k.vx.graphicsNext, placement)
}

// Destroy deletes this image from memory
func (k *KittyImage) Destroy() {
	k.vx.writeControlString(fmt.Sprintf("\x1B_Ga=d,d=I,i=%d\x1B\\", k.id))
}

func (k *KittyImage) CellSize() (w int, h int) {
	return k.w, k.h
}

func (k *KittyImage) cellSizeChanged() bool {
	cellPixW, cellPixH := k.vx.cellPixelSize()
	return k.cellPixW != cellPixW || k.cellPixH != cellPixH
}

// Resizes the image to fit within the wxh area. The image will not be
// upscaled, nor will it's aspect ratio be changed. Resizing will be done in a
// separate goroutine. A [Redraw] event will be posted when complete
func (k *KittyImage) Resize(w int, h int) {
	// Resize the image
	cellPixW, cellPixH := k.vx.cellPixelSize()
	if k.reqW == w && k.reqH == h && k.cellPixW == cellPixW && k.cellPixH == cellPixH && k.buf.Len() == 0 && atomicLoad(&k.uploaded) {
		return
	}
	k.reqW = w
	k.reqH = h
	k.cellPixW = cellPixW
	k.cellPixH = cellPixH
	img := resizeImage(k.img, w, h, cellPixW, cellPixH)

	// Reupload the image
	max := img.Bounds().Max
	k.w = max.X / cellPixW
	if max.X%cellPixW != 0 {
		k.w += 1
	}
	k.h = max.Y / cellPixH
	if max.Y%cellPixH != 0 {
		k.h += 1
	}

	atomicStore(&k.encoding, true)
	go func() {
		defer atomicStore(&k.encoding, false)
		// Encode it to base64
		buf := bytes.NewBuffer(nil)
		wc := base64.NewEncoder(base64.StdEncoding, buf)
		err := png.Encode(wc, img)
		if err != nil {
			log.Error("couldn't encode kitty image: %v", err)
			return
		}
		_ = wc.Close()
		b := make([]byte, 4096)
		atomicStore(&k.uploaded, false)
		for buf.Len() > 0 {
			n, err := buf.Read(b)
			if err == io.EOF {
				break
			}
			m := 1
			if buf.Len() == 0 {
				m = 0
			}
			fmt.Fprintf(k.buf, "\x1B_Gf=100,i=%d,m=%d;%s\x1B\\", k.id, m, string(b[:n]))
		}
		k.vx.PostEventBlocking(Redraw{})
	}()
}

// kittyRelayChunk is the payload size of one transmission chunk. The kitty
// graphics protocol caps an escape code's payload at 4096 base64 bytes.
const kittyRelayChunk = 4096

// kittyMaxRetainedPayloadBytes bounds the base64 bodies the live relays of one
// [Vaxis] pin at once.
//
// A relay holds on to its body so that a placement in a later frame can
// re-transmit it, and those bytes were produced by another process: a handful
// of images at the protocol's own per-transfer ceiling is tens of megabytes of
// base64 that no render will ever read again. Past the budget the oldest
// retained body is reclaimed, which costs that image a re-transmission and
// nothing else.
const kittyMaxRetainedPayloadBytes = 8 << 20

// kittyMaxPayloadBytes is the largest base64 body one image may be given.
//
// The ceiling has to clear the largest frame anyone would relay: a full screen
// of 32-bit pixels at 3840x2160 is 32 MiB before encoding and a little over
// 42 MiB after it, so 64 MiB is the first power of two a real image cannot
// reach. It is not a tuning knob, it is the point past which the string is not
// a frame at all, and it is what makes the newest body a relay may hold —
// which [kittyMaxRetainedPayloadBytes] deliberately exempts from reclamation
// so that an image always has bytes to draw — a bounded amount of memory.
const kittyMaxPayloadBytes = 64 << 20

// kittyMaxControlBytes is the largest control string, and the largest set of
// placement keys, one image may be given.
//
// Every key the graphics protocol defines is one or two characters with a
// short value, so a command that names all of them at once is a couple of
// hundred bytes. Anything past this is not a control string that was parsed
// out of a command, and refusing it keeps the escape code the relay writes a
// bounded multiple of the image it is placing.
const kittyMaxControlBytes = 512

// kittyControlBytes and kittyPayloadBytes are the bytes each caller-supplied
// fragment may be made of: the key names, values and separators of a control
// string, and the base64 alphabet of a body.
//
// Neither set holds ';' or ESC, which is the point. The relay splices these
// fragments into an APC escape code, so a fragment carrying either delimiter
// could end the payload early, close the escape and open another one of its
// own — and the bytes come from a process the application does not trust with
// its terminal.
//
// Tables rather than comparisons because a body is scanned end to end on the
// way in and may be tens of megabytes.
var (
	kittyControlBytes = kittyByteSet("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789=,")
	kittyPayloadBytes = kittyByteSet("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=")
)

// kittyByteSet builds the membership table for an alphabet.
func kittyByteSet(alphabet string) [256]bool {
	var set [256]bool
	for i := 0; i < len(alphabet); i++ {
		set[alphabet[i]] = true
	}
	return set
}

// checkKittyFragment reports why the relay cannot splice s into an escape code,
// or nil when it can. what names the fragment for the error message.
func checkKittyFragment(what, s string, alphabet *[256]bool, limit int) error {
	if len(s) > limit {
		return fmt.Errorf("%s is %d bytes, over the %d byte limit", what, len(s), limit)
	}
	for i := 0; i < len(s); i++ {
		if !alphabet[s[i]] {
			return fmt.Errorf("%s holds %q at byte %d", what, s[i], i)
		}
	}
	return nil
}

// kittyPayloadBudget is the retained-body accounting shared by the relays of
// one [Vaxis], which holds it as its kittyPayloads field. Its zero value is an
// empty budget.
type kittyPayloadBudget struct {
	mu    sync.Mutex
	total int
	held  []*KittyRelay // oldest retained body first
}

// KittyRelay is an image on the terminal whose bytes were produced by another
// process: a child of an embedded terminal, whose own kitty graphics commands
// are forwarded to the terminal rather than decoded into pixels.
//
// The relay owns an image id of its own ([Vaxis.ReserveGraphicID]) and nothing
// else. The control string and the base64 payload are handed to it already
// parsed and rebuilt by whoever is doing the relaying, and it never interprets
// them — but it does not trust them either. A fragment is refused unless it is
// made of the bytes its alphabet allows and fits the size its kind allows, so
// that nothing it was given can close the escape code the relay is building,
// and it writes its own a=, i=, p=, C=, q= and m= after the fragments so that
// nothing they contain can take those over.
type KittyRelay struct {
	vx *Vaxis
	id uint64

	// mu guards the fields below it, the body among them. SetTransmit runs on
	// whichever goroutine reads the other process, and the placement is
	// written during the render, so the two genuinely race: a generation and
	// the bytes behind it have to become visible together, or a render between
	// the two would transmit one generation's body under another's controls
	// and mark that generation sent.
	//
	// Lock order is vx.kittyPayloads.mu, then mu; nothing acquires them the
	// other way around.
	mu sync.Mutex
	// generation counts the bodies this image has been given. SetTransmit
	// bumps it; sentGeneration follows it as they are written.
	generation uint64
	// sentGeneration is the generation the terminal was last given. While the
	// two are equal the terminal holds the current bytes and a placement is a
	// bare a=p.
	sentGeneration uint64
	controls       string
	payload        string
	destroyed      bool
	// dropped records that this generation's body was reclaimed under the
	// budget before it could be written. There is nothing left to transmit, so
	// the placement writes nothing at all rather than an empty image.
	dropped bool
}

// storePayload replaces the retained body and keeps the running total honest.
// The caller holds vx.kittyPayloads.mu and r.mu.
func (r *KittyRelay) storePayload(body string) {
	r.vx.kittyPayloads.total += len(body) - len(r.payload)
	r.payload = body
}

// setPayload stores a body and reclaims older ones until the budget holds.
func (r *KittyRelay) setPayload(body string) {
	budget := &r.vx.kittyPayloads
	budget.mu.Lock()
	defer budget.mu.Unlock()

	r.mu.Lock()
	r.storePayload(body)
	r.mu.Unlock()
	r.retainLocked(body)
}

// retainLocked records that r holds body and reclaims older bodies until the
// budget holds. The caller holds vx.kittyPayloads.mu and no relay's mu.
func (r *KittyRelay) retainLocked(body string) {
	budget := &r.vx.kittyPayloads
	budget.held = dropKittyRelay(budget.held, r)
	if body == "" {
		return
	}
	// The body just stored goes to the back, so it is the last one reclaimed.
	budget.held = append(budget.held, r)

	// r is at the back and the loop stops at one, so the victim is never r
	// itself and taking its mu here cannot deadlock against the caller.
	for budget.total > kittyMaxRetainedPayloadBytes && len(budget.held) > 1 {
		victim := budget.held[0]
		budget.held = dropKittyRelay(budget.held, victim)
		victim.mu.Lock()
		if victim.sentGeneration != victim.generation {
			// Reclaimed before it was ever written, so this generation can
			// never be transmitted.
			victim.dropped = true
		}
		victim.storePayload("")
		victim.mu.Unlock()
	}
}

// dropKittyRelay returns relays without r. It reuses the backing array and
// clears the tail, so a relay that was dropped is not kept alive by it.
func dropKittyRelay(relays []*KittyRelay, r *KittyRelay) []*KittyRelay {
	kept := relays[:0]
	for _, relay := range relays {
		if relay == r {
			continue
		}
		kept = append(kept, relay)
	}
	for i := len(kept); i < len(relays); i++ {
		relays[i] = nil
	}
	return kept
}

// NewKittyRelay allocates an image id for an image another process produced.
// Nothing is written to the terminal until the relay is given bytes with
// [KittyRelay.SetTransmit] and placed with [KittyRelay.Place].
func (vx *Vaxis) NewKittyRelay() *KittyRelay {
	return &KittyRelay{
		vx: vx,
		id: vx.ReserveGraphicID(),
	}
}

// ID is the image id this relay was given.
func (r *KittyRelay) ID() uint64 {
	return r.id
}

// SetTransmit replaces the bytes this image is made of.
//
// controls is the comma-separated control string without a=, i= or p=, which
// the relay supplies itself; payload is the base64 body. Each call bumps the
// generation, which is what makes the next placement transmit the new bytes
// instead of re-placing the copy the terminal already holds.
//
// The controls, the body and the generation are replaced together, so a
// placement written from another goroutine sees either the whole of this call
// or none of it.
//
// An error is returned, and the image is left entirely as it was, when
// controls holds a byte that is not part of a control string, when payload
// holds a byte that is not base64, or when either is longer than its kind is
// allowed to be ([kittyMaxControlBytes], [kittyMaxPayloadBytes]). The relay
// splices both into an escape code without reading them, so the alphabets are
// what stop a fragment ending the payload or the escape early; the caller of a
// refused frame has a corrupt command and the only thing to do with it is drop
// it, which is what the terminal sees happen.
func (r *KittyRelay) SetTransmit(controls, payload string) error {
	// Checked before any lock is taken: a refused call must leave the
	// generation, the controls and the body exactly as the last accepted call
	// left them, so that a placement racing this one still writes a whole
	// image.
	if err := checkKittyFragment("kitty control string", controls, &kittyControlBytes, kittyMaxControlBytes); err != nil {
		return err
	}
	if err := checkKittyFragment("kitty payload", payload, &kittyPayloadBytes, kittyMaxPayloadBytes); err != nil {
		return err
	}

	budget := &r.vx.kittyPayloads
	budget.mu.Lock()
	defer budget.mu.Unlock()

	r.mu.Lock()
	r.controls = controls
	r.generation += 1
	r.dropped = false
	r.storePayload(payload)
	r.mu.Unlock()
	r.retainLocked(payload)
	return nil
}

// Place draws the image at win's origin, cols cells wide and rows tall, under
// the placement id pid, with placementKeys appended verbatim to the control
// string.
//
// placementKeys is the caller's rebuilt c=, r=, x=, y=, w=, h= and z=
// selection. The relay never parses it: it is a protocol fragment the caller
// assembled from the command it is relaying, and passing it through unread is
// what keeps the relay out of the business of knowing every key the protocol
// has. Two things keep that from being a way in. The keys the relay owns —
// a=, i=, p=, C=, q= and m= — are written after this fragment, and the
// terminal reads the last value of a repeated key, so nothing in here can
// redirect the transmission, rename the image, move the cursor, turn the
// terminal's replies back on or leave a chunked transfer open. And the fragment has to be a control string: an error is
// returned, and nothing is queued, when it holds a byte outside the keys,
// values and commas one is made of, or when it is longer than
// [kittyMaxControlBytes], so it cannot close this escape code and open one of
// its own.
//
// Like [KittyImage.Draw] this only queues a placement; the bytes are written
// during the render.
func (r *KittyRelay) Place(win Window, pid uint32, cols, rows int, placementKeys string) error {
	// Checked before any lock is taken, and before the placement exists, so a
	// refused call leaves the frame with exactly the placements it had.
	if err := checkKittyFragment("kitty placement keys", placementKeys, &kittyControlBytes, kittyMaxControlBytes); err != nil {
		return err
	}

	col, row := win.Origin()
	r.mu.Lock()
	generation := r.generation
	r.mu.Unlock()
	p := &placement{
		col:        col,
		row:        row,
		id:         r.id,
		w:          cols,
		h:          rows,
		generation: generation,
		writeTo: func(w io.Writer) {
			r.writePlacement(w, pid, placementKeys)
		},
		deleteFn: func(w io.Writer) {
			r.writeDelete(w, pid)
		},
	}
	r.vx.graphicsNext = append(r.vx.graphicsNext, p)
	return nil
}

// Destroy frees the image and every placement of it. The escape is written
// outside the frame, through the writer's own mutex, because the process
// behind the image may stop sending at any point and a freed image must not
// wait for the next render to be released.
func (r *KittyRelay) Destroy() {
	r.mu.Lock()
	if r.destroyed {
		r.mu.Unlock()
		return
	}
	r.destroyed = true
	r.controls = ""
	transmitted := r.sentGeneration != 0
	r.mu.Unlock()
	r.setPayload("")
	if !transmitted {
		// The terminal was never told about this id, so there is no image
		// there to free, and a delete for an id it never heard of is an escape
		// worth not writing.
		return
	}
	r.vx.deleteKittyImage(r.id)
}

// writePlacement writes either a full transmission or a bare re-placement,
// depending on whether the terminal already holds this generation's bytes.
func (r *KittyRelay) writePlacement(w io.Writer, pid uint32, placementKeys string) {
	// The budget's lock is taken before mu and held across the write, because
	// the body is reclaimed as part of transmitting it: a SetTransmit landing
	// between the two would have its bytes thrown away and its generation left
	// looking transmitted.
	budget := &r.vx.kittyPayloads
	budget.mu.Lock()
	defer budget.mu.Unlock()

	r.mu.Lock()
	switch {
	case r.destroyed, r.dropped, r.generation == 0:
		// The image is gone, or its body was reclaimed under the retained-bytes
		// budget, or it was never given any bytes at all. In none of the three
		// is there an image for the terminal to show.
		r.mu.Unlock()
		return
	case r.sentGeneration == r.generation:
		// Same bytes, so the terminal still holds the image: ask it to put the
		// one it has here again. C=1 for the same reason as the transmission
		// below.
		_, _ = fmt.Fprintf(w, "\x1B_Ga=p,i=%d,p=%d,C=1,q=2\x1B\\", r.id, pid)
		r.mu.Unlock()
		return
	}
	r.transmitLocked(w, pid, placementKeys)
	r.sentGeneration = r.generation
	// The terminal holds the bytes now, so this copy is dead weight. Reclaiming
	// it here is what keeps the steady state at zero retained bytes: the budget
	// above only has to catch bodies that were never drawn.
	r.storePayload("")
	r.mu.Unlock()
	r.retainLocked("")
}

// transmitLocked writes the image data, chunked when it does not fit one
// escape code. The caller holds mu.
func (r *KittyRelay) transmitLocked(w io.Writer, pid uint32, placementKeys string) {
	// These are the relay's own keys rather than keys it relays: the action,
	// the image id, the placement id, the cursor policy, the reply level and
	// the chunking below. They go last, after everything the caller supplied,
	// because the terminal reads the last value of a repeated key. C= in
	// particular is not the relayed process's to choose: the cursor belongs to
	// the application embedding the terminal, so a relayed image must never
	// move it, however the image it came from asked to be placed.
	relayKeys := fmt.Sprintf("a=T,i=%d,p=%d,C=1,q=2", r.id, pid)
	controls := joinKittyKeys(r.controls, placementKeys, relayKeys)
	body := r.payload
	if len(body) <= kittyRelayChunk {
		// m=0 even though nothing is chunked: an m=1 in a fragment would
		// otherwise leave the terminal waiting for chunks that never come, and
		// swallowing every image escape written after this one as if they were.
		_, _ = fmt.Fprintf(w, "\x1B_G%s,m=0;%s\x1B\\", controls, body)
		return
	}
	// Chunked: the first escape code carries the controls, every later one
	// carries only m=, and m=0 marks the last.
	first := body[:kittyRelayChunk]
	body = body[kittyRelayChunk:]
	_, _ = fmt.Fprintf(w, "\x1B_G%s,m=1;%s\x1B\\", controls, first)
	for len(body) > 0 {
		n := min(kittyRelayChunk, len(body))
		more := 1
		if n == len(body) {
			more = 0
		}
		_, _ = fmt.Fprintf(w, "\x1B_Gm=%d,q=2;%s\x1B\\", more, body[:n])
		body = body[n:]
	}
}

// writeDelete removes this image's placement when a frame no longer has it.
func (r *KittyRelay) writeDelete(w io.Writer, pid uint32) {
	r.mu.Lock()
	destroyed := r.destroyed
	transmitted := r.sentGeneration != 0
	r.mu.Unlock()
	if destroyed || !transmitted {
		// [KittyRelay.Destroy] already freed the image without waiting for a
		// render, or the terminal was never told about this id in the first
		// place. Either way there is no placement of it left to remove.
		return
	}
	_, _ = fmt.Fprintf(w, "\x1B_Ga=d,d=i,i=%d,p=%d,q=2\x1B\\", r.id, pid)
}

// joinKittyKeys concatenates control fragments with commas, skipping the empty
// ones so that an absent group cannot produce a ",," the terminal reads as a
// malformed key.
func joinKittyKeys(parts ...string) string {
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(part)
	}
	return b.String()
}

// deleteKittyImage frees a kitty image and every placement of it.
//
// The write goes through the writer's mutex rather than the frame, so it is
// safe to call from outside the render loop.
func (vx *Vaxis) deleteKittyImage(id uint64) {
	vx.writeControlString(fmt.Sprintf("\x1B_Ga=d,d=I,i=%d,q=2\x1B\\", id))
}

type Sixel struct {
	vx       *Vaxis
	img      image.Image
	buf      *bytes.Buffer
	id       uint64
	w        int
	h        int
	reqW     int
	reqH     int
	cellPixW int
	cellPixH int
	encoding int32
}

// Draw draws the [Image] to the [Window]. The image will not be drawn
// if it is larger than the window
func (s *Sixel) Draw(win Window) {
	if atomicLoad(&s.encoding) {
		return
	}
	if s.reqW != 0 && s.reqH != 0 && s.cellSizeChanged() {
		s.Resize(s.reqW, s.reqH)
		return
	}
	if s.buf.Len() == 0 {
		return
	}
	w, h := win.Size()
	if s.w > w || s.h > h {
		return
	}
	for y := 0; y < s.h; y += 1 {
		for x := 0; x < s.w; x += 1 {
			win.SetCell(x, y, Cell{
				sixel: true,
			})
		}
	}
	writeFunc := func(w io.Writer) {
		// Also need to set sixel value in here for Refresh cycles
		for y := 0; y < s.h; y += 1 {
			for x := 0; x < s.w; x += 1 {
				win.SetCell(x, y, Cell{
					sixel: true,
				})
			}
		}
		_, _ = w.Write(s.buf.Bytes())
	}
	col, row := win.Origin()
	pw, ph := s.w, s.h
	deleteFunc := func(w io.Writer) {
		for y := 0; y < ph; y += 1 {
			_, _ = fmt.Fprintf(w, "\x1b[%d;%dH%*s", row+y+1, col+1, pw, "")
		}
	}
	log.Trace("placing sixel image at cell %d,%d", col, row)
	placement := &placement{
		col:      col,
		row:      row,
		writeTo:  writeFunc,
		deleteFn: deleteFunc,
		id:       s.id,
		w:        s.w,
		h:        s.h,
	}
	s.vx.graphicsNext = append(s.vx.graphicsNext, placement)
}

// Destroy removes an image from memory. Call when done with this image
func (s *Sixel) Destroy() {
	s.buf.Reset()
}

// Resizes the image to fit within the wxh area. The image will not be
// upscaled, nor will it's aspect ratio be changed. Resize will be done in a
// separate gorotuine. A Redraw event will be posted when complete
func (s *Sixel) Resize(w int, h int) {
	cellPixW, cellPixH := s.vx.cellPixelSize()
	if s.reqW == w && s.reqH == h && s.cellPixW == cellPixW && s.cellPixH == cellPixH && s.buf.Len() != 0 {
		return
	}
	s.reqW = w
	s.reqH = h
	s.cellPixW = cellPixW
	s.cellPixH = cellPixH
	atomicStore(&s.encoding, true)
	go func() {
		defer atomicStore(&s.encoding, false)
		// Resize the image
		img := resizeImage(s.img, w, h, cellPixW, cellPixH)
		max := img.Bounds().Max
		s.w = max.X / cellPixW
		if max.X%cellPixW != 0 {
			s.w += 1
		}
		s.h = max.Y / cellPixH
		if max.Y%cellPixH != 0 {
			s.h += 1
		}
		// Re-encode the image
		s.buf.Reset()
		var paletted image.Image
		if p, ok := img.(*image.Paletted); ok && len(p.Palette) < 255 {
			// fast-path for paletted images: pass through to sixel
			paletted = p
		} else {
			paletted = octreequant.Paletted(img, 254)
		}
		err := sixel.NewEncoder(s.buf).Encode(paletted)
		if err != nil {
			log.Error("couldn't encode sixel: %v", err)
			return
		}

		// Foot requires that we set the P2 parameter = 1 in order to
		// enable transparency. This doesn't seem to affect other sixel
		// based terminals
		b := s.buf.Bytes()
		if len(b) > 4 {
			b[4] = 0x31
		}

		s.vx.PostEventBlocking(Redraw{})
	}()
}

// CellSize is the current cell size of the encoded image
func (s *Sixel) CellSize() (w int, h int) {
	if atomicLoad(&s.encoding) {
		return
	}
	return s.w, s.h
}

func (s *Sixel) cellSizeChanged() bool {
	cellPixW, cellPixH := s.vx.cellPixelSize()
	return s.cellPixW != cellPixW || s.cellPixH != cellPixH
}

func (vx *Vaxis) NewSixel(img image.Image) *Sixel {
	log.Trace("new sixel image")
	s := &Sixel{
		vx:  vx,
		img: img,
		id:  vx.nextGraphicID(),
		buf: bytes.NewBuffer(nil),
	}
	return s
}

// placement is an image placement. If two placements are identical, the
// image will not be redrawn
type placement struct {
	writeTo  func(w io.Writer)
	deleteFn func(w io.Writer)
	col      int
	row      int
	id       uint64
	w        int
	h        int
	// generation distinguishes two placements of the same image, at the same
	// cell, whose bytes differ: a relayed frame replacing its predecessor. It
	// is zero for every image this package encodes itself.
	generation uint64
}

// samePlacement compares two placements for equality. Two placements are
// considered equal if it is the same image, with the same size, at the same
// location
func samePlacement(p1, p2 *placement) bool {
	if p1.id != p2.id {
		return false
	}
	if p1.col != p2.col {
		return false
	}
	if p1.row != p2.row {
		return false
	}
	if p1.w != p2.w {
		return false
	}
	if p1.h != p2.h {
		return false
	}
	return true
}

// Resizes an image to fit within the provided rectangle (as cells). If the
// image already fits, it won't be resized
func resizeImage(img image.Image, w int, h int, cellPixW int, cellPixH int) image.Image {
	wPix := img.Bounds().Max.X
	hPix := img.Bounds().Max.Y
	// Looks complicated but we're just calculating the size of the
	// image in cells, and rounding up since we will always take
	// over any cell we bleed into.
	columns := wPix / cellPixW
	if wPix%cellPixW != 0 {
		columns += 1
	}
	lines := hPix / cellPixH
	if hPix%cellPixH != 0 {
		lines += 1
	}
	log.Debug("resizing image from (%d x %d) to (%d x %d)", columns, lines, w, h)
	if columns <= w && lines <= h {
		return img
	}
	// calculate scale factors
	sfX := float64(w) / float64(columns)
	sfY := float64(h) / float64(lines)
	newPixelWidth := wPix
	newPixelHeight := hPix
	switch {
	case sfX == sfY:
		// no-op
	case sfX < sfY:
		// Width is farther off, so set our new width to w and scale h
		// appropriately
		newPixelWidth = int(sfX * float64(wPix))
		newPixelHeight = int(sfX * float64(hPix))
	case sfX > sfY:
		newPixelWidth = int(sfY * float64(wPix))
		newPixelHeight = int(sfY * float64(hPix))
	}
	dst := image.NewRGBA(image.Rect(0, 0, newPixelWidth, newPixelHeight))
	scaleNearest(dst, img)
	return dst
}

func scaleNearest(dst *image.RGBA, src image.Image) {
	switch src := src.(type) {
	case *image.RGBA:
		scaleNearestRGBA(dst, src)
		return
	case *image.NRGBA:
		scaleNearestNRGBA(dst, src)
		return
	}
	db := dst.Bounds()
	sb := src.Bounds()
	dx := db.Dx()
	dy := db.Dy()
	sx := sb.Dx()
	sy := sb.Dy()
	for y := 0; y < dy; y++ {
		srcY := sb.Min.Y + y*sy/dy
		for x := 0; x < dx; x++ {
			srcX := sb.Min.X + x*sx/dx
			dst.Set(x+db.Min.X, y+db.Min.Y, src.At(srcX, srcY))
		}
	}
}

func scaleNearestRGBA(dst *image.RGBA, src *image.RGBA) {
	db := dst.Bounds()
	sb := src.Bounds()
	dx := db.Dx()
	dy := db.Dy()
	sx := sb.Dx()
	sy := sb.Dy()
	for y := 0; y < dy; y++ {
		srcY := sb.Min.Y + y*sy/dy
		dstOffset := dst.PixOffset(db.Min.X, db.Min.Y+y)
		for x := 0; x < dx; x++ {
			srcX := sb.Min.X + x*sx/dx
			srcOffset := src.PixOffset(srcX, srcY)
			copy(dst.Pix[dstOffset:dstOffset+4], src.Pix[srcOffset:srcOffset+4])
			dstOffset += 4
		}
	}
}

func scaleNearestNRGBA(dst *image.RGBA, src *image.NRGBA) {
	db := dst.Bounds()
	sb := src.Bounds()
	dx := db.Dx()
	dy := db.Dy()
	sx := sb.Dx()
	sy := sb.Dy()
	for y := 0; y < dy; y++ {
		srcY := sb.Min.Y + y*sy/dy
		dstOffset := dst.PixOffset(db.Min.X, db.Min.Y+y)
		for x := 0; x < dx; x++ {
			srcX := sb.Min.X + x*sx/dx
			srcOffset := src.PixOffset(srcX, srcY)
			a := uint32(src.Pix[srcOffset+3])
			dst.Pix[dstOffset+0] = uint8(uint32(src.Pix[srcOffset+0]) * a / 0xff)
			dst.Pix[dstOffset+1] = uint8(uint32(src.Pix[srcOffset+1]) * a / 0xff)
			dst.Pix[dstOffset+2] = uint8(uint32(src.Pix[srcOffset+2]) * a / 0xff)
			dst.Pix[dstOffset+3] = uint8(a)
			dstOffset += 4
		}
	}
}

// FullBlockImage is an image composed of 0x20 characters. This is the most
// primitive graphics protocol
type FullBlockImage struct {
	vx     *Vaxis
	img    image.Image
	cells  []Color
	width  int
	height int
}

func (vx *Vaxis) NewFullBlockImage(img image.Image) *FullBlockImage {
	log.Trace("new full block image")
	fb := &FullBlockImage{
		vx:  vx,
		img: img,
	}
	return fb
}

func (fb *FullBlockImage) Draw(win Window) {
	col, row := win.Origin()
	log.Trace("placing full block image at cell %d,%d", col, row)
	for i, cell := range fb.cells {
		y := i / fb.width
		x := i - (y * fb.width)
		win.SetCell(x, y, Cell{
			Character: Character{
				Grapheme: " ",
				Width:    1,
			},
			Style: Style{
				Background: cell,
			},
		})
	}
}

// Resize resizes and re-encodes an image
func (fb *FullBlockImage) Resize(w int, h int) {
	// FullBlockImage gets resized with a cell geometry of 1x2 pixels. We
	// will then average the vertical two pixels to make a single color ' '
	// character
	img := resizeImage(fb.img, w, h, 1, 2)

	// Store the actual width and height of the resized image
	fb.width = img.Bounds().Max.X
	h = img.Bounds().Max.Y
	if h%2 != 0 {
		h += 1
	}
	fb.height = h / 2
	// The image will be made into an array of cells, each cell will capture
	// 1x2 pixels
	fb.cells = make([]Color, (fb.height * fb.width))
	for i := range fb.cells {
		y := i / fb.width
		x := i - (y * fb.width)
		y *= 2

		top := img.At(x, y)
		bot := img.At(x, y+1)
		r, g, b, a := averageColor(top, bot)
		switch {
		// TODO: What is the right value for alpha that we should set
		// the background color = 0??
		case a < 50:
			fb.cells[i] = 0
		default:
			fb.cells[i] = RGBColor(r, g, b)
		}
	}
}

func (fb *FullBlockImage) Destroy() {
	fb.cells = []Color{}
}

func (fb *FullBlockImage) CellSize() (int, int) {
	return fb.width, fb.height
}

func toRGB(c color.Color) (uint8, uint8, uint8, uint8) {
	pr, pg, pb, pa := c.RGBA()
	var r, g, b, a uint8
	switch pa {
	case 0:
		r = uint8(pr)
		g = uint8(pg)
		b = uint8(pb)
	default:
		r = uint8((pr * 255) / pa)
		g = uint8((pg * 255) / pa)
		b = uint8((pb * 255) / pa)
		a = uint8(pa >> 8)
	}
	return r, g, b, a
}

// averageColor computes the average color from all inputs and returns it's rgb
// value
func averageColor(c color.Color, colors ...color.Color) (uint8, uint8, uint8, uint8) {
	var r, g, b, a int
	colors = append(colors, c)
	for _, col := range colors {
		rA, gA, bA, aA := toRGB(col)
		r += int(rA)
		g += int(gA)
		b += int(bA)
		a += int(aA)
	}
	n := len(colors)
	return uint8(r / n), uint8(g / n), uint8(b / n), uint8(a / n)
}

// HalfBlockImage is an image composed of half block characters.
type HalfBlockImage struct {
	vx     *Vaxis
	img    image.Image
	cells  []Cell
	width  int
	height int
}

func (vx *Vaxis) NewHalfBlockImage(img image.Image) *HalfBlockImage {
	log.Trace("new half block image")
	hb := &HalfBlockImage{
		vx:  vx,
		img: img,
	}
	return hb
}

func (hb *HalfBlockImage) Draw(win Window) {
	col, row := win.Origin()
	log.Trace("placing half block image at cell %d,%d", col, row)
	for i, cell := range hb.cells {
		y := i / hb.width
		x := i - (y * hb.width)
		win.SetCell(x, y, cell)
	}
}

// Resize resizes and re-encodes an image
func (hb *HalfBlockImage) Resize(w int, h int) {
	// HalfBlockImage gets resized with a cell geometry of 1x2 pixels.
	img := resizeImage(hb.img, w, h, 1, 2)

	// Store the actual width and height of the resized image
	hb.width = img.Bounds().Max.X
	h = img.Bounds().Max.Y
	if h%2 != 0 {
		h += 1
	}
	hb.height = h / 2
	// The image will be made into an array of cells, each cell will capture
	// 1x2 pixels
	hb.cells = make([]Cell, (hb.height * hb.width))
	for i := range hb.cells {
		y := i / hb.width
		x := i - (y * hb.width)
		y *= 2

		tr, tg, tb, ta := toRGB(img.At(x, y))
		br, bg, bb, ba := toRGB(img.At(x, y+1))
		// Figure out if one of the alpha channels is transparent
		// "enough"
		switch {
		case ta < transparentEnough && ba < transparentEnough:
			// Use a transparent space
			hb.cells[i] = Cell{
				Character: Character{
					Grapheme: " ",
					Width:    1,
				},
			}
		case ta < transparentEnough:
			// Top is transparent. Use a lower block
			hb.cells[i] = Cell{
				Character: Character{
					Grapheme: "▄",
					Width:    1,
				},
				Style: Style{
					Foreground: RGBColor(br, bg, bb),
				},
			}
		case ba < transparentEnough:
			// Bottom is transparent. Use an upper block
			hb.cells[i] = Cell{
				Character: Character{
					Grapheme: "▀",
					Width:    1,
				},
				Style: Style{
					Foreground: RGBColor(tr, tg, tb),
				},
			}
		default:
			// Neither is transparent. Use an upper block
			hb.cells[i] = Cell{
				Character: Character{
					Grapheme: "▀",
					Width:    1,
				},
				Style: Style{
					Foreground: RGBColor(tr, tg, tb),
					Background: RGBColor(br, bg, bb),
				},
			}
		}
	}
}

func (hb *HalfBlockImage) Destroy() {
	hb.cells = []Cell{}
}

func (hb *HalfBlockImage) CellSize() (int, int) {
	return hb.width, hb.height
}
