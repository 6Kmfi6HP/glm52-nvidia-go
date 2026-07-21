package captcha

import (
	"testing"
)

func TestBackoffForGrowsAndCaps(t *testing.T) {
	first := backoffFor(1)
	if first < backoffMin-backoffJitter || first > backoffMin+backoffJitter {
		t.Fatalf("n=1 backoff=%s want ~%s±%s", first, backoffMin, backoffJitter)
	}

	// never exceeds cap + jitter band, never negative
	for n := 1; n <= 20; n++ {
		d := backoffFor(n)
		if d < 0 {
			t.Fatalf("n=%d negative backoff=%s", n, d)
		}
		if d > backoffMax+backoffJitter {
			t.Fatalf("n=%d backoff=%s exceeds cap %s", n, d, backoffMax)
		}
	}

	// high n hits the cap band (within one jitter of the cap)
	capped := backoffFor(20)
	if capped < backoffMax-backoffJitter {
		t.Fatalf("n=20 backoff=%s did not reach cap %s", capped, backoffMax)
	}
}

