package backend

import "testing"

func TestContextRE(t *testing.T) {
	for c, want := range map[string]bool{
		"unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023": true,
		"staff_u:staff_r:staff_t:s0":                            true,
		"user_u:user_r:user_t:s0":                               true,
		"selinuxdefcon: error":                                  false,
		"unconfined_u:unconfined_r:unconfined_t:s0 extra":       false,
		"": false,
	} {
		if contextRE.MatchString(c) != want {
			t.Errorf("%q: got %v", c, !want)
		}
	}
}
