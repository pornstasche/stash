package manager

import (
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"sort"

	"github.com/lucasb-eyer/go-colorful"
	"github.com/stashapp/stash/pkg/logger"
)

type InteractiveHeatmapSpeedGenerator struct {
	sceneDurationMilli int64
	InteractiveSpeed   int
	Funscript          Script
	FunscriptPath      string
	HeatmapPath        string
	Width              int
	Height             int
	NumSegments        int
}

type Script struct {
	// Version of Launchscript
	Version string `json:"version"`
	// Inverted causes up and down movement to be flipped.
	Inverted bool `json:"inverted,omitempty"`
	// Range is the percentage of a full stroke to use.
	Range int `json:"range,omitempty"`
	// Actions are the timed moves.
	Actions      []Action `json:"actions"`
	AvarageSpeed int64
}

// Action is a move at a specific time.
type Action struct {
	// At time in milliseconds the action should fire.
	At int64 `json:"at"`
	// Pos is the place in percent to move to.
	Pos int `json:"pos"`

	Slope     float64
	Intensity int64
	Speed     float64
}

type GradientTable []struct {
	Col    colorful.Color
	Pos    float64
	YRange [2]float64
}

func NewInteractiveHeatmapSpeedGenerator(funscriptPath string, heatmapPath string, sceneDuration float64) *InteractiveHeatmapSpeedGenerator {
	return &InteractiveHeatmapSpeedGenerator{
		sceneDurationMilli: int64(sceneDuration * 1000),
		FunscriptPath:      funscriptPath,
		HeatmapPath:        heatmapPath,
		Width:              1280,
		Height:             60,
		NumSegments:        600,
	}
}

func (g *InteractiveHeatmapSpeedGenerator) Generate() error {
	funscript, err := g.LoadFunscriptData(g.FunscriptPath)

	if err != nil {
		return err
	}

	if len(funscript.Actions) == 0 {
		return fmt.Errorf("no valid actions in funscript")
	}

	g.Funscript = funscript
	g.Funscript.UpdateIntensityAndSpeed()

	err = g.RenderHeatmap()

	if err != nil {
		return err
	}

	g.InteractiveSpeed = g.Funscript.CalculateMedian()

	return nil
}

func (g *InteractiveHeatmapSpeedGenerator) LoadFunscriptData(path string) (Script, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Script{}, err
	}

	var funscript Script
	err = json.Unmarshal(data, &funscript)
	if err != nil {
		return Script{}, err
	}

	if funscript.Actions == nil {
		return Script{}, fmt.Errorf("actions list missing in %s", path)
	}

	sort.SliceStable(funscript.Actions, func(i, j int) bool { return funscript.Actions[i].At < funscript.Actions[j].At })

	// trim actions with negative timestamps to avoid index range errors when generating heatmap
	// #3181 - also trim actions that occur after the scene duration
	loggedBadTimestamp := false
	isValid := func(x int64) bool {
		return x >= 0 && x < g.sceneDurationMilli
	}

	i := 0
	for _, x := range funscript.Actions {
		if isValid(x.At) {
			funscript.Actions[i] = x
			i++
		} else if !loggedBadTimestamp {
			loggedBadTimestamp = true
			logger.Warnf("Invalid timestamp %d in %s: subsequent invalid timestamps will not be logged", x.At, path)
		}
	}

	funscript.Actions = funscript.Actions[:i]

	return funscript, nil
}

func (funscript *Script) UpdateIntensityAndSpeed() {

	var t1, t2 int64
	var p1, p2 int
	var slope float64
	var intensity int64
	for i := range funscript.Actions {
		if i == 0 {
			continue
		}
		t1 = funscript.Actions[i].At
		t2 = funscript.Actions[i-1].At
		p1 = funscript.Actions[i].Pos
		p2 = funscript.Actions[i-1].Pos

		slope = math.Min(math.Max(1/(2*float64(t1-t2)/1000), 0), 20)
		intensity = int64(slope * math.Abs((float64)(p1-p2)))
		speed := math.Abs(float64(p1-p2)) / float64(t1-t2) * 1000

		funscript.Actions[i].Slope = slope
		funscript.Actions[i].Intensity = intensity
		funscript.Actions[i].Speed = speed
	}
}

// funscript needs to have intensity updated first
func (g *InteractiveHeatmapSpeedGenerator) RenderHeatmap() error {

	gradient := g.Funscript.getGradientTable(g.NumSegments)

	img := image.NewRGBA(image.Rect(0, 0, g.Width, g.Height))
	for x := 0; x < g.Width; x++ {
		xPos := float64(x) / float64(g.Width)
		c := gradient.GetInterpolatedColorFor(xPos)
		yRange := gradient.GetYRange(xPos)
		top := int(yRange[0] / 100.0 * float64(g.Height))
		bottom := int(yRange[1] / 100.0 * float64(g.Height))
		draw.Draw(img, image.Rect(x, g.Height-top, x+1, g.Height-bottom), &image.Uniform{c}, image.Point{}, draw.Src)
	}

	// add 10 minute marks
	maxts := g.Funscript.Actions[len(g.Funscript.Actions)-1].At
	const tick = 600000
	var ts int64 = tick
	c, _ := colorful.Hex("#000000")
	for ts < maxts {
		x := int(float64(ts) / float64(maxts) * float64(g.Width))
		draw.Draw(img, image.Rect(x-1, g.Height/2, x+1, g.Height), &image.Uniform{c}, image.Point{}, draw.Src)
		ts += tick
	}

	outpng, err := os.Create(g.HeatmapPath)
	if err != nil {
		return err
	}
	defer outpng.Close()

	err = png.Encode(outpng, img)
	return err
}

func (funscript *Script) CalculateMedian() int {
	sort.Slice(funscript.Actions, func(i, j int) bool {
		return funscript.Actions[i].Speed < funscript.Actions[j].Speed
	})

	mNumber := len(funscript.Actions) / 2

	if len(funscript.Actions)%2 != 0 {
		return int(funscript.Actions[mNumber].Speed)
	}

	return int((funscript.Actions[mNumber-1].Speed + funscript.Actions[mNumber].Speed) / 2)
}

func (gt GradientTable) GetInterpolatedColorFor(t float64) colorful.Color {
	for i := 0; i < len(gt)-1; i++ {
		c1 := gt[i]
		c2 := gt[i+1]
		if c1.Pos <= t && t <= c2.Pos {
			// We are in between c1 and c2. Go blend them!
			t := (t - c1.Pos) / (c2.Pos - c1.Pos)
			return c1.Col.BlendHcl(c2.Col, t).Clamped()
		}
	}

	// Nothing found? Means we're at (or past) the last gradient keypoint.
	return gt[len(gt)-1].Col
}

func (gt GradientTable) GetYRange(t float64) [2]float64 {
	for i := 0; i < len(gt)-1; i++ {
		c1 := gt[i]
		c2 := gt[i+1]
		if c1.Pos <= t && t <= c2.Pos {
			// TODO: We are in between c1 and c2. Go blend them!
			return c1.YRange
		}
	}

	// Nothing found? Means we're at (or past) the last gradient keypoint.
	return gt[len(gt)-1].YRange
}

func (funscript Script) getGradientTable(numSegments int) GradientTable {
	const windowSize = 15
	const backfillThreshold = 500

	segments := make([]struct {
		count     int
		intensity int
		yRange    [2]float64
		at        int64
	}, numSegments)
	gradient := make(GradientTable, numSegments)
	posList := []int{}

	maxts := funscript.Actions[len(funscript.Actions)-1].At

	for _, a := range funscript.Actions {
		posList = append(posList, a.Pos)

		if len(posList) > windowSize {
			posList = posList[1:]
		}

		sortedPos := make([]int, len(posList))
		copy(sortedPos, posList)
		sort.Ints(sortedPos)

		topHalf := sortedPos[len(sortedPos)/2:]
		bottomHalf := sortedPos[0 : len(sortedPos)/2]

		var totalBottom int = 0
		var totalTop int = 0

		for _, value := range bottomHalf {
			totalBottom += value
		}
		for _, value := range topHalf {
			totalTop += value
		}

		averageBottom := float64(totalBottom) / float64(len(bottomHalf))
		averageTop := float64(totalTop) / float64(len(topHalf))

		segment := int(float64(a.At) / float64(maxts+1) * float64(numSegments))
		// #3181 - sanity check. Clamp segment to numSegments-1
		if segment >= numSegments {
			segment = numSegments - 1
		}
		segments[segment].at = a.At
		segments[segment].count++
		segments[segment].intensity += int(a.Intensity)
		segments[segment].yRange[0] = averageTop
		segments[segment].yRange[1] = averageBottom
	}

	lastSegment := segments[0]

	// Fill in gaps in segments
	for i := 0; i < numSegments; i++ {
		segmentTS := int64(float64(i) / float64(numSegments))

		// Empty segment - fill it with the previous up to backfillThreshold ms
		if segments[i].count == 0 {
			if segmentTS-lastSegment.at < backfillThreshold {
				segments[i].count = lastSegment.count
				segments[i].intensity = lastSegment.intensity
				segments[i].yRange[0] = lastSegment.yRange[0]
				segments[i].yRange[1] = lastSegment.yRange[1]
			}
		} else {
			lastSegment = segments[i]
		}
	}

	for i := 0; i < numSegments; i++ {
		gradient[i].Pos = float64(i) / float64(numSegments-1)
		gradient[i].YRange = segments[i].yRange
		if segments[i].count > 0 {
			gradient[i].Col = getSegmentColor(float64(segments[i].intensity) / float64(segments[i].count))
		} else {
			gradient[i].Col = getSegmentColor(0.0)
		}
	}

	return gradient
}

func getSegmentColor(intensity float64) colorful.Color {
	colorBlue, _ := colorful.Hex("#1e90ff")   // DodgerBlue
	colorGreen, _ := colorful.Hex("#228b22")  // ForestGreen
	colorYellow, _ := colorful.Hex("#ffd700") // Gold
	colorRed, _ := colorful.Hex("#dc143c")    // Crimson
	colorPurple, _ := colorful.Hex("#800080") // Purple
	colorBlack, _ := colorful.Hex("#0f001e")
	colorBackground, _ := colorful.Hex("#30404d") // Same as GridCard bg

	var stepSize = 60.0
	var f float64
	var c colorful.Color

	switch {
	case intensity <= 0.001:
		c = colorBackground
	case intensity <= 1*stepSize:
		f = (intensity - 0*stepSize) / stepSize
		c = colorBlue.BlendLab(colorGreen, f)
	case intensity <= 2*stepSize:
		f = (intensity - 1*stepSize) / stepSize
		c = colorGreen.BlendLab(colorYellow, f)
	case intensity <= 3*stepSize:
		f = (intensity - 2*stepSize) / stepSize
		c = colorYellow.BlendLab(colorRed, f)
	case intensity <= 4*stepSize:
		f = (intensity - 3*stepSize) / stepSize
		c = colorRed.BlendRgb(colorPurple, f)
	default:
		f = (intensity - 4*stepSize) / (5 * stepSize)
		f = math.Min(f, 1.0)
		c = colorPurple.BlendLab(colorBlack, f)
	}

	return c
}
