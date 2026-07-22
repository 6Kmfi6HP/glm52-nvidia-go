package captcha

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsHardExtractFailure(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("temporary glitch"), false},
		{fmt.Errorf("empty captcha token — headless Chrome may be blocked"), true},
		{fmt.Errorf("sticky execute failed (x); re-navigate failed: y"), true},
		{fmt.Errorf("hcaptcha global not ready (bot detection or page change?)"), true},
		{fmt.Errorf("chromedp navigate: timeout"), true},
		{fmt.Errorf("captcha token did not refresh after execute({async:true})"), true},
	}
	for _, tc := range cases {
		if got := isHardExtractFailure(tc.err); got != tc.want {
			t.Fatalf("isHardExtractFailure(%v)=%v want %v", tc.err, got, tc.want)
		}
	}
}
