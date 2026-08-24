package httpapi

import "testing"

func TestIsCacheKeySegment(t *testing.T) {
	cases := []struct {
		name string
		seg  string
		want bool
	}{
		{"plain id", "Lxr9tvYUHcg_1080_hls", true},
		{"id with embedded underscore", "GMJ_E1nnIoo_1080_hls", true},
		{"id with two embedded underscores", "a_b_c1080hls9_1080_mp4", true},
		{"mp4 container", "Lxr9tvYUHcg_720_mp4", true},
		{"unknown container", "Lxr9tvYUHcg_1080_webm", false},
		{"unknown quality", "Lxr9tvYUHcg_999_hls", false},
		{"no underscores", "Lxr9tvYUHcg", false},
		{"empty id", "_1080_hls", false},
		{"missing quality", "Lxr9tvYUHcg_hls", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCacheKeySegment(c.seg); got != c.want {
				t.Errorf("isCacheKeySegment(%q) = %v, want %v", c.seg, got, c.want)
			}
		})
	}
}
