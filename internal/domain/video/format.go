package video

import (
	"fmt"
	"strconv"
)

// Container is the delivery format. AVPro takes HLS; Unity VideoPlayer
// needs progressive MP4 (spec §4.2.1).
type Container string

const (
	ContainerHLS Container = "hls"
	ContainerMP4 Container = "mp4"
)

// ParseContainer maps a URL extension onto a Container.
func ParseContainer(ext string) (Container, bool) {
	switch ext {
	case "m3u8", "hls":
		return ContainerHLS, true
	case "mp4":
		return ContainerMP4, true
	}
	return "", false
}

// QualityCap is an upper bound on output height, never an exact demand:
// packaging picks the best available format at or below it (spec §4.1.2).
type QualityCap int

var allowedQualities = []QualityCap{360, 480, 720, 1080, 1440, 2160}

// ParseQuality validates s as one of the permitted quality caps.
func ParseQuality(s string) (QualityCap, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidQuality, s)
	}
	for _, q := range allowedQualities {
		if QualityCap(n) == q {
			return q, nil
		}
	}
	return 0, fmt.Errorf("%w: %d", ErrInvalidQuality, n)
}

// Clamp lowers the cap to max, enforcing MAX_QUALITY (spec §8) so a URL
// cannot ask for more than the operator allows.
func (q QualityCap) Clamp(max QualityCap) QualityCap {
	if q > max {
		return max
	}
	return q
}

// OutputSpec identifies one deliverable variant of a video.
type OutputSpec struct {
	Container Container  `json:"container"`
	Quality   QualityCap `json:"quality"`
}

// FormatSelector renders the yt-dlp format expression for this spec.
//
// avc1+mp4a is forced because it is the one combination AVPro and Unity
// VideoPlayer both hardware-decode everywhere, and because holding the
// codec fixed is what lets ffmpeg run -c copy instead of transcoding
// (spec §4.2.2).
func (s OutputSpec) FormatSelector() string {
	h := int(s.Quality)
	return fmt.Sprintf(
		"bv*[height<=%d][vcodec^=avc1]+ba[acodec^=mp4a]/bv*[height<=%d][vcodec^=avc1]+ba/b[height<=%d]",
		h, h, h)
}

// CacheKey names one packaged artifact. Different qualities and
// containers are genuinely different artifacts, so each caches
// separately (spec §4.7.1).
type CacheKey string

func (s OutputSpec) CacheKey(id ID) CacheKey {
	return CacheKey(fmt.Sprintf("%s_%d_%s", id, s.Quality, s.Container))
}
