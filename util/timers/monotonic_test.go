// Copyright (C) 2019-2025 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package timers

import (
	"math/rand"
	"testing"
	"time"

	"github.com/algorand/go-algorand/test/partitiontest"
)

func polled(ch <-chan time.Time) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestMonotonicDelta(t *testing.T) {
	partitiontest.PartitionTest(t)

	var m Monotonic[int]
	var c Clock[int]
	var ch <-chan time.Time

	d := time.Millisecond * 100

	c = m.Zero()
	ch = c.TimeoutAt(d, 0)
	if polled(ch) {
		t.Errorf("channel fired ~100ms early")
	}

	<-time.After(d * 2)
	if !polled(ch) {
		t.Errorf("channel failed to fire at 100ms")
	}

	ch = c.TimeoutAt(d/2, 0)
	if !polled(ch) {
		t.Errorf("channel failed to fire at 50ms")
	}
}

func TestMonotonicZeroDelta(t *testing.T) {
	partitiontest.PartitionTest(t)

	var m Monotonic[int]
	var c Clock[int]
	var ch <-chan time.Time

	c = m.Zero()
	ch = c.TimeoutAt(0, 0)
	if !polled(ch) {
		t.Errorf("read failed on channel at zero timeout")
	}
}

func TestMonotonicNegativeDelta(t *testing.T) {
	partitiontest.PartitionTest(t)

	var m Monotonic[int]
	var c Clock[int]
	var ch <-chan time.Time

	c = m.Zero()
	ch = c.TimeoutAt(-time.Second, 0)
	if !polled(ch) {
		t.Errorf("read failed on channel at negative timeout")
	}
}

func TestMonotonicZeroTwice(t *testing.T) {
	partitiontest.PartitionTest(t)

	var m Monotonic[int]
	var c Clock[int]
	var ch <-chan time.Time

	d := time.Millisecond * 100

	c = m.Zero()
	ch = c.TimeoutAt(d, 0)
	if polled(ch) {
		t.Errorf("channel fired ~100ms early")
	}

	<-time.After(d * 2)
	if !polled(ch) {
		t.Errorf("channel failed to fire at 100ms")
	}

	c = c.Zero()
	ch = c.TimeoutAt(d, 0)
	if polled(ch) {
		t.Errorf("channel fired ~100ms early after call to Zero")
	}

	<-time.After(d * 2)
	if !polled(ch) {
		t.Errorf("channel failed to fire at 100ms after call to Zero")
	}
}

func TestMonotonicEncodeDecode(t *testing.T) {
	partitiontest.PartitionTest(t)

	singleTest := func(c Clock[int], descr string) {
		data := c.Encode()
		c0, err := c.Decode(data)
		if err != nil {
			t.Errorf("decoding error: %v", err)
		}
		if !time.Time(c.(*Monotonic[int]).zero).Equal(time.Time(c0.(*Monotonic[int]).zero)) {
			t.Errorf("%v clock not encoded properly: %v != %v", descr, c, c0)
		}
	}

	var c Clock[int]
	var m Monotonic[int]

	c = Clock[int](&m)
	singleTest(c, "empty")

	c = c.Zero()
	singleTest(c, "Zero()'ed")

	now := time.Now()
	for i := 0; i < 100; i++ {
		r := time.Duration(rand.Int63())
		c = Clock[int](
			&Monotonic[int]{
				zero: now.Add(r),
			},
		)
		singleTest(c, "random")
	}
}

func TestTimeoutTypes(t *testing.T) {
	partitiontest.PartitionTest(t)

	var m Monotonic[int]
	var c Clock[int]

	d := time.Millisecond * 100

	c = m.Zero()
	ch1 := c.TimeoutAt(d, 0)
	ch2 := c.TimeoutAt(d, 1)
	if polled(ch1) {
		t.Errorf("channel fired ~100ms early")
	}
	if polled(ch2) {
		t.Errorf("channel fired ~100ms early")
	}

	if ch1 == ch2 {
		t.Errorf("equal channels for different timeout types")
	}

	<-time.After(d * 2)
	if !polled(ch1) {
		t.Errorf("channel failed to fire at 100ms")
	}
	if !polled(ch2) {
		t.Errorf("channel failed to fire at 100ms")
	}

	ch1 = c.TimeoutAt(d/2, 0)
	if !polled(ch1) {
		t.Errorf("channel failed to fire at 50ms")
	}
	ch2 = c.TimeoutAt(d/2, 0)
	if !polled(ch2) {
		t.Errorf("channel failed to fire at 50ms")
	}
}
