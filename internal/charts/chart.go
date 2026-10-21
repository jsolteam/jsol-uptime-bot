package charts

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"time"

	"github.com/jsolteam/jsol-uptime-bot/internal/storage"
)

func Render(metric string, samples []storage.Sample) ([]byte, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("нет измерений за период")
	}
	const width, height = 960, 560
	const left, right, top, bottom = 92, 934, 68, 476
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	background := color.RGBA{R: 250, G: 251, B: 253, A: 255}
	axis := color.RGBA{R: 70, G: 82, B: 98, A: 255}
	grid := color.RGBA{R: 222, G: 228, B: 236, A: 255}
	series := color.RGBA{R: 35, G: 110, B: 210, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: background}, image.Point{}, draw.Src)

	for value := 0; value <= 100; value += 20 {
		y := bottom - value*(bottom-top)/100
		line(img, left, y, right, y, grid)
		drawText(img, 8, y-7, fmt.Sprintf("%d%%", value), 2, axis)
	}
	line(img, left, top, left, bottom, axis)
	line(img, left, bottom, right, bottom, axis)

	tickCount := min(4, len(samples)-1)
	timeFormat := "15:04"
	if samples[len(samples)-1].CheckedAt.Sub(samples[0].CheckedAt) >= 24*time.Hour {
		timeFormat = "02.01"
	}
	for tick := 0; tick <= tickCount; tick++ {
		index, x := 0, left
		if tickCount > 0 {
			index = tick * (len(samples) - 1) / tickCount
			x += tick * (right - left) / tickCount
		}
		line(img, x, top, x, bottom, grid)
		label := samples[index].CheckedAt.Local().Format(timeFormat)
		drawText(img, x-30, bottom+24, label, 2, axis)
	}

	var previous image.Point
	previousValid := false
	for i, sample := range samples {
		value, valid := metricValue(metric, sample)
		if !valid {
			previousValid = false
			continue
		}
		x := left
		if len(samples) > 1 {
			x += i * (right - left) / (len(samples) - 1)
		}
		value = math.Max(0, math.Min(100, value))
		point := image.Pt(x, bottom-int(value*float64(bottom-top)/100))
		if previousValid {
			line(img, previous.X, previous.Y, point.X, point.Y, series)
		}
		dot(img, point, series)
		previous = point
		previousValid = true
	}

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func metricValue(metric string, sample storage.Sample) (float64, bool) {
	switch metric {
	case "cpu":
		return sample.CPU, sample.AgentStatus == "AVAILABLE"
	case "ram":
		if sample.RAMTotal == 0 || sample.AgentStatus != "AVAILABLE" {
			return 0, false
		}
		return sample.RAMUsed / sample.RAMTotal * 100, true
	case "availability":
		switch sample.Status {
		case "UP":
			return 100, true
		case "DOWN":
			return 0, true
		default:
			return 0, false
		}
	default:
		return 0, false
	}
}

func drawText(img *image.RGBA, x, y int, text string, scale int, c color.RGBA) {
	for _, char := range text {
		glyph, ok := font[char]
		if !ok {
			x += 6 * scale
			continue
		}
		for row, bits := range glyph {
			for col, bit := range bits {
				if bit != '1' {
					continue
				}
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						img.Set(x+col*scale+dx, y+row*scale+dy, c)
					}
				}
			}
		}
		x += 6 * scale
	}
}

var font = map[rune][7]string{
	'0': {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "10000", "11110", "00001", "00001", "11110"},
	'6': {"01110", "10000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	'%': {"11001", "11010", "00100", "01000", "10110", "00110", "00000"},
	':': {"00000", "00100", "00100", "00000", "00100", "00100", "00000"},
	'.': {"00000", "00000", "00000", "00000", "00000", "00110", "00110"},
}

func dot(img *image.RGBA, p image.Point, c color.RGBA) {
	for dx := -3; dx <= 3; dx++ {
		for dy := -3; dy <= 3; dy++ {
			if dx*dx+dy*dy <= 9 {
				img.Set(p.X+dx, p.Y+dy, c)
			}
		}
	}
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
