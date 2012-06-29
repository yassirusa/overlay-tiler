// Copyright (c) Google Inc. All Rights Reserved.
// Author: Chris Broadfoot (cbro@google.com)
// Author: Andrew Gerrand (adg@google.com)

package overlaytiler

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"

	"appengine"
	"appengine/blobstore"
	"appengine/channel"
	"appengine/datastore"
	"appengine/taskqueue"
	"appengine/user"

	"code.google.com/p/graphics-go/graphics"
)

func init() {
	// User-facing HTTP handlers.
	http.Handle("/", appHandler(rootHandler))
	http.Handle("/download", appHandler(downloadHandler))
	http.Handle("/overlays.json", appHandler(listHandler))
	http.Handle("/process", appHandler(processHandler))
	http.Handle("/upload", appHandler(uploadHandler))
}

var rootTemplate = template.Must(template.ParseFiles("templates/root.html"))

type rootTemplateData struct {
	LogoutURL string
	UploadURL string
	User      string
}

// rootHandler returns the landing page, which includes a blobstore upload URL.
func rootHandler(c appengine.Context, w http.ResponseWriter, r *http.Request) *appError {
	logoutURL, err := user.LogoutURL(c, "/")
	if err != nil {
		c.Warningf("creating logout URL: %v", err)
		logoutURL = "/"
	}
	uploadURL, err := blobstore.UploadURL(c, "/upload", nil)
	if err != nil {
		return appErrorf(err, "could not create blobstore upload url")
	}
	username := "none"
	if u := user.Current(c); u != nil {
		username = u.String()
	}
	err = rootTemplate.Execute(w, &rootTemplateData{
		LogoutURL: logoutURL,
		UploadURL: uploadURL.String(),
		User:      username,
	})
	if err != nil {
		return appErrorf(err, "could not write template")
	}
	return nil
}

// uploadHandler handles the image upload and stores a new Overlay in the
// datastore. If successful, it writes the Overlay's key to the response.
func uploadHandler(c appengine.Context, w http.ResponseWriter, r *http.Request) *appError {
	// Handle the upload, and get the image's BlobKey.
	blobs, _, err := blobstore.ParseUpload(r)
	if err != nil {
		return appErrorf(err, "could not parse blobs from blobstore upload")
	}
	b := blobs["overlay"]
	if len(b) < 1 {
		return appErrorf(nil, "could not find overlay blob")
	}
	bk := b[0].BlobKey

	// Fetch image from blob store to find its width and height.
	m, err := imageBlob(c, bk)
	if err != nil {
		return appErrorf(err, "could not get image")
	}

	// Create and store a new Overlay in the datastore.
	o := &Overlay{
		Owner:  user.Current(c).ID,
		Image:  bk,
		Width:  m.Bounds().Dx(),
		Height: m.Bounds().Dy(),
	}
	k := datastore.NewIncompleteKey(c, "Overlay", nil)
	k, err = datastore.Put(c, k, o)
	if err != nil {
		return appErrorf(err, "could not save new overlay to datastore")
	}

	// It will be known hereafter by its datastore-provided key.
	fmt.Fprintf(w, "%s", k.Encode())
	return nil
}

// processHandler initiates the processing of an Overlay, including kicking off
// appropriate slice tasks.
func processHandler(c appengine.Context, w http.ResponseWriter, r *http.Request) *appError {
	if r.Method != "POST" {
		return &appError{nil, "must use POST", http.StatusMethodNotAllowed}
	}

	// Get the Overlay from the datastore.
	k, o, err := getOverlay(r)
	if err != nil {
		return appErrorf(err, "overlay not found")
	}

	// Process the request.
	if o.TopLeft, err = parsePair(r.FormValue("topLeft")); err != nil {
		return appErrorf(err, "invalid parameter topLeft")
	}
	if o.TopRight, err = parsePair(r.FormValue("topRight")); err != nil {
		return appErrorf(err, "invalid parameter topLeft")
	}
	if o.BottomRight, err = parsePair(r.FormValue("bottomRight")); err != nil {
		return appErrorf(err, "invalid parameter bottomRight")
	}

	// Compute the transformation matrix.
	a := graphics.I.Scale(1/float64(o.Width), 1/float64(o.Height)).
		Mul(inverse(graphics.Affine{
		o.TopRight[0] - o.TopLeft[0], o.BottomRight[0] - o.TopRight[0], o.TopLeft[0],
		o.TopRight[1] - o.TopLeft[1], o.BottomRight[1] - o.TopRight[1], o.TopLeft[1],
		0, 0, 1,
	}))
	o.Transform = []float64(a[:])

	// TODO(cbro): get min/max zoom from user.
	o.MinZoom = 0
	o.MaxZoom = 21

	// Compute tiles to be generated.
	var tiles []*Tile
	for zoom := o.MinZoom; zoom <= o.MaxZoom; zoom++ {
		tiles = append(tiles, tilesForZoom(o, zoom)...)
	}
	o.Tiles = len(tiles)

	// Create a channel between the app and the client's browser.
	token, err := channel.Create(c, k.Encode())
	if err != nil {
		return appErrorf(err, "couldn't create browser channel")
	}

	// Put the updated Overlay into the datastore.
	if _, err := datastore.Put(c, k, o); err != nil {
		return appErrorf(err, "could not save overlay to datastore")
	}

	// Create tasks to generate tiles.
	tasks := tileTasks(k.Encode(), tiles)
	if err := addTasks(c, tasks, tileQueue); err != nil {
		return appErrorf(err, "could not start tiling process")
	}

	// Create task to start slice process.
	task := taskqueue.NewPOSTTask("/slice", url.Values{"key": {k.Encode()}})
	for i := 0; i < sliceBackends; i++ {
		host := appengine.BackendHostname(c, sliceBackend, i)
		task.Header.Set("Host", host)
		if _, err := taskqueue.Add(c, task, sliceQueue); err != nil {
			return appErrorf(err, "could not start tiling process")
		}
	}

	// Send channel token as response.
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(token))
	return nil
}

// tilesForZoom returns a slice of Tiles at the specified zoom level.  If the
// number of tiles to be generated is too large (greater than tilesPerZoom),
// an empty slice is returned.
func tilesForZoom(o *Overlay, zoom int64) (tiles []*Tile) {
	l := scaleCoord(min(o.TopLeft[0], o.TopRight[0], o.BottomRight[0], o.BottomLeft()[0]), zoom)
	r := scaleCoord(max(o.TopLeft[0], o.TopRight[0], o.BottomRight[0], o.BottomLeft()[0]), zoom)
	t := scaleCoord(min(o.TopLeft[1], o.TopRight[1], o.BottomRight[1], o.BottomLeft()[1]), zoom)
	b := scaleCoord(max(o.TopLeft[1], o.TopRight[1], o.BottomRight[1], o.BottomLeft()[1]), zoom)

	if (r-l+1)*(b-t+1) > tilesPerZoom {
		return
	}

	for x := l; x <= r; x++ {
		for y := t; y <= b; y++ {
			tiles = append(tiles, &Tile{X: x, Y: y, Zoom: zoom})
		}
	}
	return
}

// tileTasks returns tasks to generate the provided Tiles.
func tileTasks(key string, tiles []*Tile) (tasks []*taskqueue.Task) {
	for _, tile := range tiles {
		b, err := json.Marshal(tile)
		if err != nil {
			panic(err)
		}
		task := &taskqueue.Task{
			Method:  "PULL",
			Tag:     key,
			Payload: b,
		}
		tasks = append(tasks, task)
	}
	return
}

// downloadHandler serves the zip file generated by zipHandler.
func downloadHandler(c appengine.Context, w http.ResponseWriter, r *http.Request) *appError {
	k, o, err := getOverlay(r)
	if err != nil {
		return appErrorf(err, "overlay not found")
	}
	if o.Zip == "" || o.Zip == zipSentinel {
		return appErrorf(nil, "overlay's zip not generated yet")
	}
	attachment := fmt.Sprintf(`attachment;filename="%s.zip"`, k.Encode())
	w.Header().Add("Content-Disposition", attachment)
	blobstore.Send(w, o.Zip)
	return nil
}

// listHandler returns a JSON-encoded list of Overlays that belong to the
// logged-in user.
func listHandler(c appengine.Context, w http.ResponseWriter, r *http.Request) *appError {
	q := datastore.NewQuery("Overlay").
		Filter("Owner = ", user.Current(c).ID)

	var overlays []*Overlay
	if _, err := q.GetAll(c, &overlays); err != nil {
		return appErrorf(err, "could not get overlays")
	}

	if err := json.NewEncoder(w).Encode(overlays); err != nil {
		return appErrorf(err, "could not marshal overlay json")
	}
	return nil
}
