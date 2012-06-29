// Copyright (c) Google Inc. All Rights Reserved.
// Author: Chris Broadfoot (cbro@google.com)

package overlaytiler

import (
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"appengine"
	"appengine/blobstore"
	"appengine/datastore"
	"appengine/taskqueue"

	"code.google.com/p/graphics-go/graphics"
)

// inverse returns the inverse 3x3 matrix of a graphics.Affine.
func inverse(a graphics.Affine) graphics.Affine {
	a11 := a[0]
	a12 := a[1]
	a13 := a[2]
	a21 := a[3]
	a22 := a[4]
	a23 := a[5]
	a31 := a[6]
	a32 := a[7]
	a33 := a[8]

	invdet := 1 / (a11*a22*a33 + a21*a32*a13 + a31*a12*a23 - a11*a32*a23 - a31*a22*a13 - a21*a12*a33)

	return graphics.Affine{
		(a22*a33 - a23*a32) * invdet, (a13*a32 - a12*a33) * invdet, (a12*a23 - a13*a22) * invdet,
		(a23*a31 - a21*a33) * invdet, (a11*a33 - a13*a31) * invdet, (a13*a21 - a11*a23) * invdet,
		(a21*a32 - a22*a31) * invdet, (a12*a31 - a11*a32) * invdet, (a11*a22 - a12*a21) * invdet,
	}
}

func max(n ...float64) (r float64) {
	r = n[0]
	for i := 1; i < len(n); i++ {
		if n[i] > r {
			r = n[i]
		}
	}
	return r
}

func min(n ...float64) (r float64) {
	r = n[0]
	for i := 1; i < len(n); i++ {
		if n[i] < r {
			r = n[i]
		}
	}
	return r
}

// getOverlay fetches an Overlay (identified by the "key" form value)
// from the datastore.
func getOverlay(r *http.Request) (*datastore.Key, *Overlay, error) {
	c := appengine.NewContext(r)
	ks := r.FormValue("key")
	k, err := datastore.DecodeKey(ks)
	if err != nil {
		return nil, nil, err
	}
	o := new(Overlay)
	if err := datastore.Get(c, k, o); err != nil {
		return nil, nil, err
	}
	return k, o, nil
}

// scaleCoord converts a magnitude from mercator pixels to tile coordinates at
// a specified zoom level.
func scaleCoord(p float64, zoom int64) (t int64) {
	return int64(p * math.Pow(2, float64(zoom)) / 256) // floor
}

// parsePair parses a comma separated pair of floats into a []float64.
func parsePair(s string) ([]float64, error) {
	n := strings.Split(s, ",")
	if len(n) != 2 {
		return nil, errors.New("point needs to be two numbers, comma-separated")
	}
	pair := make([]float64, 2)
	var err error
	for i := 0; i < 2 && err == nil; i++ {
		pair[i], err = strconv.ParseFloat(n[i], 64)
	}
	return pair, err
}

type appHandler func(appengine.Context, http.ResponseWriter, *http.Request) *appError

func (fn appHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	if e := fn(c, w, r); e != nil {
		http.Error(w, e.Message, e.Code)
		c.Errorf("%s (%v)", e.Message, e.Error)
	}
}

type appError struct {
	Error   error
	Message string
	Code    int
}

func appErrorf(err error, format string, v ...interface{}) *appError {
	return &appError{err, fmt.Sprintf(format, v...), 500}
}

// createBlob stores a blob in the blobstore.
func createBlob(c appengine.Context, r io.Reader, contentType string) (appengine.BlobKey, error) {
	w, err := blobstore.Create(c, contentType)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(w, r); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return w.Key()
}

// imageBlob fetches the specified blob and decodes it as an image.
func imageBlob(c appengine.Context, k appengine.BlobKey) (image.Image, error) {
	r := blobstore.NewReader(c, k)
	m, _, err := image.Decode(r)
	return m, err
}

// addTasks adds the provided tasks in batches of 100 or less.
// This is to sidestep a limitation in the taskqueue API.
func addTasks(c appengine.Context, tasks []*taskqueue.Task, queue string) error {
	n := 100
	for len(tasks) > 0 {
		if len(tasks) < n {
			n = len(tasks)
		}
		_, err := taskqueue.AddMulti(c, tasks[:n], queue)
		if err != nil {
			return err
		}
		tasks = tasks[n:]
	}
	return nil
}
