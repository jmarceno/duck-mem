package main

import (
	"image"
	"image/color"
	"testing"
)

func TestResampleAreaAveragesInPremultipliedAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{}) // transparent
	src.SetNRGBA(0, 1, color.NRGBA{B: 255, A: 255})
	src.SetNRGBA(1, 1, color.NRGBA{G: 255, A: 255})

	// Alpha-weighted average: the transparent pixel contributes nothing, so the
	// colour stays 1/3 red, 1/3 blue, 1/3 green at 3/4 coverage. Averaging the
	// channels unweighted would give 1/4 each at 3/4 coverage.
	got := resampleArea(src, 1).NRGBAAt(0, 0)
	want := color.NRGBA{R: 85, G: 85, B: 85, A: 191}
	if got != want {
		t.Errorf("resample 2x2 to 1px = %v, want %v", got, want)
	}
}

func TestTrayIconPixmapsMatchTheirDeclaredSize(t *testing.T) {
	set, err := newTrayIcons()
	if err != nil {
		t.Fatalf("render tray icons: %v", err)
	}
	for name, list := range map[string][]iconPixmap{"logo": set.logo, "busy": set.busy, "stopped": set.stopped} {
		if len(list) == 0 {
			t.Fatalf("%s: no pixmaps", name)
		}
		for _, p := range list {
			if got, want := len(p.Data), int(p.Width)*int(p.Height)*4; got != want {
				t.Errorf("%s %dx%d: %d bytes, want %d", name, p.Width, p.Height, got, want)
			}
		}
	}
	// The smallest logo must still be the artwork: an opaque duck inside a
	// transparent margin, not a square block.
	small := pickPixmap(set.logo, 22)
	var solid, clear int
	for i := 0; i < len(small.Data); i += 4 {
		switch a := int(small.Data[i]); {
		case a == 0:
			clear++
		case a >= 250: // the artwork's flat fills sit just under 255
			solid++
		}
	}
	if solid == 0 || clear == 0 {
		t.Errorf("22px logo: %d solid and %d transparent pixels, want both", solid, clear)
	}
}

func TestOverlayOnlyMarksTheStoppedOrBusyStates(t *testing.T) {
	set := trayIcons{busy: []iconPixmap{{Width: 1}}, stopped: []iconPixmap{{Width: 1}}}
	if got := set.overlay(traySnapshot{running: true}); len(got) != 0 {
		t.Errorf("running overlay = %d pixmaps, want none", len(got))
	}
	if got := set.overlay(traySnapshot{running: true, busy: true}); len(got) != 1 || got[0].Width != 1 {
		t.Errorf("busy overlay = %v, want the busy badge", got)
	}
	if got := set.overlay(traySnapshot{}); len(got) != 1 || got[0].Width != 1 {
		t.Errorf("stopped overlay = %v, want the stopped badge", got)
	}
}
