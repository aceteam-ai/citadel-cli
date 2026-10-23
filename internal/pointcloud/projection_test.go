package pointcloud

import (
	"math"
	"reflect"
	"testing"
)

func TestProjectionUsesClampedDistanceAxesMaskAndEvenSampling(t *testing.T) {
	distance := []float32{1, 2, 3, float32(math.NaN())}
	rgb := []byte{255, 0, 0, 0, 255, 0, 0, 0, 255, 255, 255, 255}
	options := Projection{HorizontalFOV: 90, Near: 0.5, Far: 2, PointBudget: 2}
	points, err := Project(2, 2, distance, rgb, nil, options)
	want := []Point{{-0.5, 0.5, -1, 255, 0, 0}, {-1, -1, -2, 0, 0, 255}}
	if err != nil || !reflect.DeepEqual(points, want) {
		t.Fatalf("synthetic projection differs: %v", err)
	}
	points, err = Project(2, 2, distance, rgb, []bool{false, true, false, false}, options)
	if err != nil || !reflect.DeepEqual(points, []Point{{1, 1, -2, 0, 255, 0}}) {
		t.Fatal("mask selected the wrong RGB/depth pixel")
	}
	options.PointBudget = 1
	points, err = Project(2, 2, distance, rgb, nil, options)
	if err != nil || !reflect.DeepEqual(points, []Point{{1, 1, -2, 0, 255, 0}}) {
		t.Fatal("single-point budget must choose the middle eligible pixel")
	}
	points, err = Project(2, 2, []float32{0, -1, float32(math.NaN()), float32(math.Inf(1))}, rgb, nil, options)
	if err != nil || len(points) != 0 {
		t.Fatal("invalid distances were not skipped")
	}
}

func TestProjectionRefusesShapeAndPolicyErrors(t *testing.T) {
	good := Projection{HorizontalFOV: 60, Near: 0.1, Far: 100, PointBudget: 150000}
	for _, change := range []func(*Projection){
		func(p *Projection) { p.HorizontalFOV = 0 },
		func(p *Projection) { p.HorizontalFOV = 180 },
		func(p *Projection) { p.HorizontalFOV = math.NaN() },
		func(p *Projection) { p.Near = 0 },
		func(p *Projection) { p.Far = p.Near },
		func(p *Projection) { p.Far = math.Inf(1) },
		func(p *Projection) { p.Far = 65505 },
		func(p *Projection) { p.PointBudget = 0 },
		func(p *Projection) { p.PointBudget = MaxFramePoints + 1 },
	} {
		options := good
		change(&options)
		if _, err := Project(1, 1, []float32{1}, []byte{1, 2, 3}, nil, options); err != ErrInvalid {
			t.Fatal("invalid projection options accepted")
		}
	}
	for _, shape := range [][2]int{{0, 1}, {-1, 1}, {4097, 1}, {1, 4097}, {2, 2}} {
		if _, err := Project(shape[0], shape[1], []float32{1}, []byte{1, 2, 3}, nil, good); err != ErrInvalid {
			t.Fatal("invalid/mismatched frame shape accepted")
		}
	}
	if _, err := Project(1, 1, []float32{1}, []byte{1, 2, 3}, []bool{}, good); err != ErrInvalid {
		t.Fatal("non-nil mask must match frame dimensions")
	}
}
