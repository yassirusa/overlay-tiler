// Copyright (c) Google Inc. All Rights Reserved.
// Author: Chris Broadfoot (cbro@google.com)
// Author: Andrew Gerrand (adg@google.com)

package overlaytiler

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/url"
	"time"

	"appengine"
	"appengine/channel"
	"appengine/datastore"
	"appengine/taskqueue"

	"code.google.com/p/graphics-go/graphics"
	"code.google.com/p/graphics-go/graphics/interp"

	"timer"
)

func init() {
	// Task handlers.
	http.Handle("/slice", appHandler(sliceHandler))
	http.Handle("/zip", appHandler(zipHandler))
}

// sliceHandler fetches Tile tasks from the tileQueue, 
// generates and stores an image for each Tile, and - if all the
// tiles have been generated - kicks off the zip task.
func sliceHandler(c appengine.Context, w http.ResponseWriter, r *http.Request) *appError {
	tim := timer.New()

	tim.Point("process request")

	// Get Overlay from datastore and its Image from blobstore.
	k, o, err := getOverlay(r)
	if err != nil {
		return appErrorf(err, "overlay not found")
	}

	tim.Point("get Overlay")

	m, err := imageBlob(c, o.Image)
	if err != nil {
		return appErrorf(err, "could not get image")
	}

	tim.Point("get overlay Image")

restart:
	const (
		inFlight   = 10 // tiles to process at once
		secPerTile = 2  // worst-case time to process one tile
	)

	errc := make(chan error, o.Tiles/inFlight+1)
	errs := 0
	count := 0

	// Generate images for the provided tiles.
	for {
		tasks, err := taskqueue.LeaseByTag(c, inFlight, tileQueue, inFlight*secPerTile, k.Encode())
		if err != nil {
			return appErrorf(err, "couldn't get more tasks")
		}
		if len(tasks) == 0 {
			// No more work to do.
			break
		}
		tiles := make([]*Tile, len(tasks))
		keys := make([]*datastore.Key, len(tasks))
		for i, task := range tasks {
			tile := new(Tile)
			err = json.Unmarshal(task.Payload, tile)
			if err != nil {
				panic(err)
			}
			err = slice(c, tile, o.Transform, m)
			if err != nil {
				panic(err)
			}
			tiles[i] = tile
			keys[i] = tile.Key(c, k)
		}

		// Store generated tiles in datastore while going back to
		// generate more.
		go func() {
			_, err = datastore.PutMulti(c, keys, tiles)
			if err != nil {
				errc <- err
				return
			}
			for _, task := range tasks {
				err = taskqueue.Delete(c, task, tileQueue)
				if err != nil {
					errc <- err
					return
				}
			}
			var ids []string
			for _, t := range tiles {
				ids = append(ids, t.String())
			}
			send(c, k.Encode(), Message{Total: o.Tiles, IDs: ids})
			count += len(tiles)
			errc <- nil
		}()
		errs++
	}

	// Wait for all goroutines to finish.
	for i := 0; i < errs; i++ {
		if e := <-errc; err == nil && e != nil {
			err = e
		}
	}
	if err != nil {
		return appErrorf(err, "could not generate tiles")
	}

	tim.Pointf("generate and put %d tiles", count)

	// Start zip task if we're done.
	done, err := checkDone(c, k)
	if err != nil {
		return appErrorf(err, "could not check job status")
	}

	tim.Point("checkDone")

	// If we're not done we must have missed some tasks.
	// Wait a second and try it all over again.
	if !done {
		c.Infof("Not done; trying again")
		time.Sleep(1 * time.Second)
		goto restart
	}

	// Tell the client we're done.
	send(c, k.Encode(), Message{TilesDone: true})

	c.Infof("%v", tim)

	return nil
}

// slice draws the specified tile using the given transformation and source
// image and stores it in the provided Tile's Image field.
func slice(c appengine.Context, tile *Tile, transform []float64, m image.Image) error {
	// Convert the transformation matrix to a graphics.Affine.
	var a graphics.Affine
	copy(a[:], transform)

	// Scale and translate the matrix for this Tile's coordinates.
	s := math.Pow(2, float64(tile.Zoom))
	a = a.Scale(s, s).Translate(float64(-tile.X*256), float64(-tile.Y*256))

	// Allocate the target image and draw the transformation into it.
	m2 := image.NewRGBA(image.Rect(0, 0, 256, 256))
	a.Transform(m2, m, interp.Bilinear)

	// Generate PNG-encoded image and store it in the Image field.
	buf := new(bytes.Buffer)
	err := png.Encode(buf, m2)
	if err != nil {
		return err
	}
	tile.Image = buf.Bytes()
	return nil
}

// checkDone tests whether we have generated all the tiles.
// If so, it creates a zip task on the zipper backend.
func checkDone(c appengine.Context, oKey *datastore.Key) (done bool, err error) {
	// Create a zip task if we're done and one hasn't been created already.
	tx := func(c appengine.Context) error {
		o := new(Overlay)
		err := datastore.Get(c, oKey, o)
		if err != nil {
			return err
		}

		// Check whether we have generated all the tiles.
		count, err := datastore.NewQuery("Tile").Ancestor(oKey).Count(c)
		if err != nil {
			return err
		}
		done = o.Tiles >= count
		if !done || o.Zip != "" {
			return nil
		}

		// Create a task to build the zip file,
		// targeting the zipper backend.
		task := taskqueue.NewPOSTTask("/zip", url.Values{
			"key": {oKey.Encode()},
		})
		if !appengine.IsDevAppServer() {
			host := appengine.BackendHostname(c, zipBackend, -1)
			task.Header.Set("Host", host)
		}
		if _, err := taskqueue.Add(c, task, zipQueue); err != nil {
			return err
		}

		// Store a sentinel value in Zip field to prevent a
		// second zip task from being created.
		// This value will be overwritten by the zip task.
		o.Zip = zipSentinel
		_, err = datastore.Put(c, oKey, o)
		return err
	}
	if err := datastore.RunInTransaction(c, tx, nil); err != nil {
		return false, err
	}
	return done, nil
}

// zipHandler creates a zip file containing all tile images and an index.html
// containing a Maps API tile overlay, writes it to blobstore, and updates
// stores the BlobKey in the Overlay.
func zipHandler(c appengine.Context, w http.ResponseWriter, r *http.Request) *appError {
	k, o, err := getOverlay(r)
	if err != nil {
		return appErrorf(err, "overlay not found")
	}

	// Create a zip file, writing its contents to a buffer.
	buf := new(bytes.Buffer)
	z := zip.NewWriter(buf)

	// Add the tiles.
	if err := addTilesToZip(c, z, k); err != nil {
		return appErrorf(err, "could not add tile images to zip file")
	}

	// Generate and add the index.html file.
	if err := addIndexToZip(c, z, k, o); err != nil {
		return appErrorf(err, "could not generate index.html")
	}

	// Finish writing the zip file.
	if err := z.Close(); err != nil {
		return appErrorf(err, "could not close zip")
	}

	// Store the buffer in blobstore and update the overlay.
	o.Zip, err = createBlob(c, buf, "application/zip")
	if err != nil {
		return appErrorf(err, "could not store zip file")
	}
	if _, err := datastore.Put(c, k, o); err != nil {
		return appErrorf(err, "could not store overlay")
	}

	// Tell the client we're done.
	send(c, k.Encode(), Message{ZipDone: true})

	return nil
}

// addTilesToZip fetches all the Tile records for a given Overlay, fetches
// their associated image blobs, and adds them to the provided zip file.
func addTilesToZip(c appengine.Context, z *zip.Writer, oKey *datastore.Key) error {
	base := oKey.Encode()
	q := datastore.NewQuery("Tile").Ancestor(oKey)
	for i := q.Run(c); ; {
		var t Tile
		if _, err := i.Next(&t); err == datastore.Done {
			break
		} else if err != nil {
			return err
		}
		name := fmt.Sprintf("%s/%d/%d/%d.png", base, t.Zoom, t.X, t.Y)
		w, err := z.Create(name)
		if err != nil {
			return err
		}
		if _, err = w.Write(t.Image); err != nil {
			return err
		}
	}
	return nil
}

// addIndexToZip generates an index.html file for the given Overlay and adds
// it to the provided zip file.
func addIndexToZip(c appengine.Context, z *zip.Writer, oKey *datastore.Key, o *Overlay) error {
	w, err := z.Create(fmt.Sprintf("%s/index.html", oKey.Encode()))
	if err != nil {
		return err
	}
	return zipTemplate.Execute(w, o)
}

var zipTemplate = template.Must(template.New("zip.html").Funcs(template.FuncMap{
	"coord": coordString,
}).ParseFiles("templates/zip.html"))

// coordString returns a string representation of two float64 coordinates.
func coordString(p []float64) template.JS {
	if len(p) < 2 {
		return "bad coordinates"
	}
	return template.JS(fmt.Sprintf("%f, %f", p[0], p[1]))
}

// send uses the Channel API to send the provided message in JSON-encoded form
// to the client identified by clientID.
//
// Channels created with one version of an app (eg, the default frontend)
// cannot be sent on from another version (eg, a backend). This is a limitation
// of the Channel API that should be fixed at some point.
// The send function creates a task that runs on the frontend (where the
// channel was created). The task handler makes the channel.Send API call.
func send(c appengine.Context, clientID string, m Message) {
	if clientID == "" {
		c.Debugf("no channel; skipping message send")
		return
	}
	switch {
	case m.TilesDone:
		c.Debugf("tiles done")
	case m.ZipDone:
		c.Debugf("zip done")
	default:
		c.Debugf("%d tiles", len(m.IDs))
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	task := taskqueue.NewPOSTTask("/send", url.Values{
		"clientID": {clientID},
		"msg":      {string(b)},
	})
	host := appengine.DefaultVersionHostname(c)
	task.Header.Set("Host", host)
	if _, err := taskqueue.Add(c, task, sendQueue); err != nil {
		c.Errorf("add send task failed: %v", err)
	}
}

func init() {
	http.HandleFunc("/send", sendHandler)
}

func sendHandler(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	id := r.FormValue("clientID")
	msg := r.FormValue("msg")
	if err := channel.Send(c, id, msg); err != nil {
		c.Errorf("channel send failed: %v", err)
	}
}
