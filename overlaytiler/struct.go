// Copyright (c) Google Inc. All Rights Reserved.
// Author: Chris Broadfoot (cbro@google.com)

package overlaytiler

import (
	"fmt"

	"appengine"
	"appengine/datastore"
)

const (
	tilesPerZoom = 1000 // limit to prevent DoS

	sliceQueue = "slice"
	tileQueue  = "tile"
	zipQueue   = "zip"
	sendQueue  = "send"

	sliceBackend  = "slicer"
	sliceBackends = 4
	zipBackend    = "zipper"

	zipSentinel = "ZIP_RUNNING"
)

// Overlay describes a map overlay image and the state of the tile generation
// process. It is to be stored in the datastore. The presence of a valid
// BlobKey in the Zip field indicates the process is complete.
type Overlay struct {
	Owner  string            // User ID of the creator of this Overlay.
	Image  appengine.BlobKey // Overlay image location.
	Width  int               // Overlay image dimensions.
	Height int

	TopLeft     []float64 // Position of the overlay in world coordinates.
	TopRight    []float64
	BottomRight []float64
	Transform   []float64 // Overlay image transformation matrix.
	MinZoom     int64     // Zoom limits.
	MaxZoom     int64
	Tiles       int // Total number of Tiles to generate.

	Zip appengine.BlobKey // Zip file location.
}

// BottomLeft calculates the bottom-left point of the overlay, based on
// TopLeft, BottomRight, and TopRight. The resulting quad is a parallelogram.
func (o *Overlay) BottomLeft() (p []float64) {
	p = make([]float64, 2)
	for i := 0; i < 2; i++ {
		p[i] = o.TopLeft[i] + o.BottomRight[i] - o.TopRight[i]
	}
	return
}

// Tile represents a single tile, it is a child of Overlay.
type Tile struct {
	Image      []byte `json:"-"`
	X, Y, Zoom int64  // tile coordinates
}

func (t *Tile) String() string {
	return fmt.Sprintf("%d,%d,%d", t.X, t.Y, t.Zoom)
}

func (t *Tile) Key(c appengine.Context, parent *datastore.Key) *datastore.Key {
	return datastore.NewKey(c, "Tile", t.String(), 0, parent)
}

// Message is the data structure that is sent (in JSON-encoded form) to the
// client via the Channel API.
type Message struct {
	Total int
	IDs   []string

	TilesDone bool
	ZipDone   bool
}
