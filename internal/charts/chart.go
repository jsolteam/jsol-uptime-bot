package charts

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"github.com/jsolteam/jsol-uptime-bot/internal/storage"
)

func Render(metric string, samples []storage.Sample) ([]byte, error) {
	const width, height = 900, 420
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 250, G: 251, B: 253, A: 255}}, image.Point{}, draw.Src)
	axis := color.RGBA{R: 90, G: 100, B: 115, A: 255}
	grid := color.RGBA{R: 225, G: 230, B: 237, A: 255}
	for i := 0; i < 5; i++ {
		y := 60 + i*75
		line(img, 60, y, width-25, y, grid)
	}
	line(img, 60, 50, 60, 360, axis)
	line(img, 60, 360, width-25, 360, axis)
	if len(samples) == 0 {
		return nil, fmt.Errorf("нет измерений за период")
	}
	points := make([]image.Point, len(samples))
	for i, s := range samples {
		x := 60
		if len(samples) > 1 {
			x = 60 + i*(width-90)/(len(samples)-1)
		}
		v := math.Max(0, math.Min(100, value(metric, s)))
		points[i] = image.Pt(x, 360-int(v*3))
	}
	c := color.RGBA{R: 35, G: 110, B: 210, A: 255}
	for i := 1; i < len(points); i++ {
		line(img, points[i-1].X, points[i-1].Y, points[i].X, points[i].Y, c)
	}
	for _, p := range points {
		for dx := -3; dx <= 3; dx++ {
			for dy := -3; dy <= 3; dy++ {
				if dx*dx+dy*dy <= 9 {
					img.Set(p.X+dx, p.Y+dy, c)
				}
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
func value(metric string, s storage.Sample) float64 {
	switch metric {
	case "cpu":
		return s.CPU
	case "ram":
		if s.RAMTotal == 0 {
			return 0
		}
		return s.RAMUsed / s.RAMTotal * 100
	case "availability":
		if s.Status == "UP" {
			return 100
		}
	}
	return 0
}
func line(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx := int(math.Abs(float64(x1 - x0)))
	sx := -1
	if x0 < x1 {
		sx = 1
	}
	dy := -int(math.Abs(float64(y1 - y0)))
	sy := -1
	if y0 < y1 {
		sy = 1
	}
	err := dx + dy
	for {
		if image.Pt(x0, y0).In(img.Bounds()) {
			img.Set(x0, y0, c)
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e := 2 * err
		if e >= dy {
			err += dy
			x0 += sx
		}
		if e <= dx {
			err += dx
			y0 += sy
		}
	}
}
