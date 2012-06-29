// Copyright (c) Google Inc. All Rights Reserved.
// Author: Andrew Gerrand (adg@google.com)

package timer

import (
	"bytes"
	"fmt"
	"time"
)

type Timer struct {
	start time.Time
	point []time.Duration
	label []string
}

func New() *Timer {
	return &Timer{start: time.Now()}
}

// Point adds a opint to the Timer, associating the duration since the last
// point (or the construction of the timer) with the provided label.
func (t *Timer) Point(label string) {
	n := time.Now()
	d := n.Sub(t.start)
	t.point = append(t.point, d)
	t.label = append(t.label, label)
	t.start = n
}

// Pointf is like Point but it uses fmt.Sprintf to format the label.
func (t *Timer) Pointf(format string, args ...interface{}) {
	t.Point(fmt.Sprintf(format, args...))
}

func (t *Timer) String() string {
	b := new(bytes.Buffer)
	for i := range t.point {
		fmt.Fprintf(b, "%v: %v\n", t.label[i], t.point[i])
	}
	return b.String()
}
