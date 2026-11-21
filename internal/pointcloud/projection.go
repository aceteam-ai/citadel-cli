package pointcloud

import "math"

// Projection requires already converted positive camera distance, not raw
// relative/inverse model output. It does not establish metric calibration.
type Projection struct {
	HorizontalFOV float64
	Near, Far     float64
	PointBudget   int
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Project uses pixel centres, +X right, +Y up, -Z forward. It skips masked,
// nonpositive/nonfinite samples, clamps other distances and deterministically
// samples eligible row-major pixels. Memory is bounded by PointBudget.
func Project(width, height int, distance []float32, rgb []byte, mask []bool, options Projection) ([]Point, error) {
	if width < 1 || height < 1 || width > 4096 || height > 4096 || width*height > 16777216 ||
		len(distance) != width*height || len(rgb) != 3*width*height || mask != nil && len(mask) != width*height ||
		!finite(options.HorizontalFOV) || options.HorizontalFOV <= 0 || options.HorizontalFOV >= 180 ||
		!finite(options.Near) || !finite(options.Far) || options.Near <= 0 || options.Near >= options.Far ||
		options.Far > 65504 || options.PointBudget < 1 || options.PointBudget > MaxFramePoints {
		return nil, ErrInvalid
	}
	eligible := func(i int) bool {
		return (mask == nil || mask[i]) && distance[i] > 0 && finite(float64(distance[i]))
	}
	count := 0
	for i := range distance {
		if eligible(i) {
			count++
		}
	}
	want := min(count, options.PointBudget)
	points := make([]Point, 0, want)
	if want == 0 {
		return points, nil
	}
	target := func(selected int) int {
		if want == 1 {
			return (count - 1) / 2
		}
		return int(int64(selected) * int64(count-1) / int64(want-1))
	}
	focal := float64(width) / (2 * math.Tan(options.HorizontalFOV*math.Pi/360))
	if !finite(focal) || focal <= 0 {
		return nil, ErrInvalid
	}
	seen, next := 0, target(0)
	for i, d := range distance {
		if !eligible(i) {
			continue
		}
		if seen == next {
			z := max(options.Near, min(float64(d), options.Far))
			x := (float64(i%width) + 0.5 - float64(width)/2) * z / focal
			y := -(float64(i/width) + 0.5 - float64(height)/2) * z / focal
			p := Point{float32(x), float32(y), float32(-z), rgb[3*i], rgb[3*i+1], rgb[3*i+2]}
			if !finiteCoordinate(p.X) || !finiteCoordinate(p.Y) || !finiteCoordinate(p.Z) {
				return nil, ErrInvalid
			}
			points = append(points, p)
			if len(points) == want {
				break
			}
			next = target(len(points))
		}
		seen++
	}
	return points, nil
}
