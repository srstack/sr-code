package main

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func writeTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestValidateImage_SuccessReturnsDimsJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chart.png")
	writeTestPNG(t, p, 640, 480)

	text, isErr := validateImage(p)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}
	var got struct {
		Message string `json:"message"`
		W       int    `json:"w"`
		H       int    `json:"h"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("result is not JSON: %q (%v)", text, err)
	}
	if got.W != 640 || got.H != 480 {
		t.Errorf("dims = %dx%d, want 640x480", got.W, got.H)
	}
	if got.Message == "" {
		t.Error("expected a human-facing message field")
	}
}

func TestValidateImage_Errors(t *testing.T) {
	cases := []struct{ name, path string }{
		{"empty", ""},
		{"not an image", filepath.Join(t.TempDir(), "notes.txt")},
		{"missing", filepath.Join(t.TempDir(), "nope.png")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, isErr := validateImage(c.path)
			if !isErr {
				t.Errorf("expected isError for %q", c.path)
			}
		})
	}
}
