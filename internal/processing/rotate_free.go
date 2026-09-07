package processing

// rotateFree applies a rotation by an angle that is not a multiple of 90.
//
// It is a separate step from rotateAndFlip, and runs after cropToResult, for one
// reason: a free rotation changes the canvas size, and every size the pipeline
// computed up front — ScaledWidth, ResultCropWidth and the rest — describes the
// image *before* it. Rotating earlier would leave cropToResult cutting the
// rotated frame back to the requested box and shearing off the corners the
// rotation just created, which is not what `resize,m_fill,w_100,h_100/rotate,45`
// asks for.
//
// This step is an iStore addition; imgproxy only rotates by multiples of 90.
func (p *Processor) rotateFree(c *Context) error {
	angle := c.PO.RotateFree()
	if angle == 0 {
		return nil
	}

	// The rotation reads the whole frame in random order.
	if err := c.Img.CopyMemory(); err != nil {
		return err
	}

	return c.Img.RotateAny(angle)
}
