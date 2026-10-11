package weed_server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A HEAD is answered from the needle's metadata, except when the response is
// a resized or cropped image: that is built from the needle's data, and its
// Content-Length is the changed image's.
func TestShouldAttemptStreamWrite(t *testing.T) {
	for _, tc := range []struct {
		name         string
		method       string
		target       string
		ext          string
		hasVolume    bool
		wantAttempt  bool
		wantMustMeta bool
	}{
		{"HEAD", http.MethodHead, "/1,abc.jpg", ".jpg", true, true, true},
		{"GET", http.MethodGet, "/1,abc.jpg", ".jpg", true, true, false},
		{"HEAD of a resized image", http.MethodHead, "/1,abc.jpg?width=100", ".jpg", true, false, false},
		{"GET of a resized image", http.MethodGet, "/1,abc.jpg?width=100", ".jpg", true, false, false},
		{"HEAD of a cropped image", http.MethodHead, "/1,abc.png?crop_x2=10&crop_y2=10", ".png", true, false, false},
		{"HEAD with a width that does not apply", http.MethodHead, "/1,abc.txt?width=100", ".txt", true, true, true},
		{"HEAD of an upper-case extension", http.MethodHead, "/1,abc.JPG?width=100", ".JPG", true, false, false},
		{"HEAD without a local volume", http.MethodHead, "/1,abc.jpg", ".jpg", false, false, false},
	} {
		r := httptest.NewRequest(tc.method, tc.target, nil)
		attempt, mustMeta := shouldAttemptStreamWrite(tc.hasVolume, tc.ext, r)
		if attempt != tc.wantAttempt || mustMeta != tc.wantMustMeta {
			t.Errorf("%s: attempt %v, must be metadata only %v; want %v, %v", tc.name, attempt, mustMeta, tc.wantAttempt, tc.wantMustMeta)
		}
	}
}
