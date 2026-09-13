package term

import (
	"image"

	"go.rockorager.dev/vaxis"
)

type Image struct {
	origin struct {
		row int
		col int
	}
	sourceRow int
	rows      int
	cols      int
	img       image.Image
	vaxii     []*vaxisImage
	// kitty is set when this entry is not an image at all but a PLACEMENT of
	// a host-side image the child transmitted. img stays nil for those: the
	// pixels live in the host terminal, never here.
	kitty *kittyPlacement
}

type positionedImage struct {
	img *Image
	row int
	col int
}

type vaxisImage struct {
	// A handle on the vaxis that created this image. This is in case
	// multiple vaxis instances are connected to the same term widget
	vx      *vaxis.Vaxis
	vxImage vaxis.Image
}

func (img *Image) destroy() {
	for _, cached := range img.vaxii {
		cached.vxImage.Destroy()
	}
	img.vaxii = nil
	if img.kitty != nil {
		// Every path in term.go that drops an *Image goes through here, so
		// this is where a relayed placement stops being drawn -- without any
		// of those paths having to know about the relay.
		img.kitty.retire(img)
		img.kitty = nil
	}
}
