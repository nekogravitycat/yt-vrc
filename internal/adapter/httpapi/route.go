package httpapi

import (
	"net/url"
	"strings"

	"github.com/nekogravitycat/yt-vrc/internal/domain/video"
)

// RouteKind distinguishes the two things a path can mean.
type RouteKind int

const (
	RouteVideo RouteKind = iota
	RouteCommand
)

// Route is the parsed meaning of a request path.
type Route struct {
	Kind    RouteKind
	VideoID video.ID
	Spec    video.OutputSpec
	Command string   // for RouteCommand, the canonical long name
	Args    []string // remaining path segments after the command
	// ContainerExplicit reports whether the URL named a container
	// (a .mp4/.m3u8 extension) rather than falling back to a default.
	// Message delivery has its own default, so it has to tell the two
	// apart -- see messageSpec.
	ContainerExplicit bool
}

// messageSpec is the output spec a message video answering this route
// should be delivered as. A message is a still frame, so its container
// tradeoff is the opposite of a real video's (see
// Defaults.MessageContainer) -- but an explicit extension on the URL
// still wins, exactly as it does for playback.
func (r Route) messageSpec(d Defaults) video.OutputSpec {
	spec := r.Spec
	if !r.ContainerExplicit && d.MessageContainer != "" {
		spec.Container = d.MessageContainer
	}
	return spec
}

// commandAliases maps every accepted spelling onto a canonical name.
// Every command is reachable by a short and a long form (spec §4.1.3).
var commandAliases = map[string]string{
	"s": "status", "status": "status",
	"u": "upgrade", "upgrade": "upgrade",
	"h": "help", "help": "help",
	"l": "list", "list": "list",
	"e": "errors", "errors": "errors",
	"p": "purge", "purge": "purge",
	"on": "enable", "enable": "enable",
	"off": "disable", "disable": "disable",
	"w": "warm", "warm": "warm",
	"r": "refresh", "refresh": "refresh",
	"i": "info", "info": "info",
	"d": "drop", "drop": "drop",
	"m": "mode", "mode": "mode",
}

// Defaults supplies the settings a path may omit.
type Defaults struct {
	Container  video.Container
	Quality    video.QualityCap
	MaxQuality video.QualityCap
	// MessageContainer is the container for command responses and error
	// frames when the URL didn't name one. Empty falls back to Container.
	MessageContainer video.Container
}

// ParsePath resolves a request path in the strict order of spec §4.1.4.
//
// The order matters: a YouTube ID is always exactly 11 characters, so no
// command keyword can ever collide with one, which is what allows
// commands to sit at the URL root alongside bare video IDs.
func ParsePath(p string, d Defaults) (Route, error) {
	spec := video.OutputSpec{Container: d.Container, Quality: d.Quality}
	trimmed := strings.Trim(p, "/")

	// 1. Root serves help.
	if trimmed == "" {
		return Route{Kind: RouteCommand, Command: "help", Spec: spec}, nil
	}

	// 4. A full YouTube URL, pasted straight after the host. Checked
	// before segment splitting because such a URL contains slashes;
	// restoreSchemeSlash undoes net/http's path collapse first (CRITICAL,
	// see below).
	trimmed = restoreSchemeSlash(trimmed)
	if lower := strings.ToLower(trimmed); strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		id, err := extractIDFromURL(trimmed)
		if err != nil {
			return Route{}, err
		}
		return Route{Kind: RouteVideo, VideoID: id, Spec: spec}, nil
	}

	// Only the pasted-URL branch above wants a query string; for every
	// other form it is noise.
	if i := strings.IndexByte(trimmed, '?'); i >= 0 {
		trimmed = trimmed[:i]
	}

	segs := strings.Split(trimmed, "/")
	head, ext := splitExt(segs[0])

	// 2. An 11-character segment is a video ID.
	if id, err := video.ParseID(head); err == nil {
		s, explicit, err := specFromSegments(segs[1:], ext, d)
		if err != nil {
			return Route{}, err
		}
		return Route{Kind: RouteVideo, VideoID: id, Spec: s, ContainerExplicit: explicit}, nil
	}

	// 3. A registered command keyword.
	if canon, ok := commandAliases[strings.ToLower(head)]; ok {
		s, explicit, err := specFromSegments(nil, ext, d)
		if err != nil {
			return Route{}, err
		}
		return Route{Kind: RouteCommand, Command: canon, Args: segs[1:], Spec: s, ContainerExplicit: explicit}, nil
	}

	// 5. Nothing matched.
	return Route{}, video.ErrInvalidVideoID
}

// specFromSegments applies an optional trailing quality segment and an
// optional extension, e.g. /{id}/720.mp4 (spec §4.1.2). The second
// result reports whether a container was named anywhere in the path,
// which is what lets message delivery apply its own default without
// overriding a URL that asked for one (see Route.messageSpec).
func specFromSegments(rest []string, ext string, d Defaults) (video.OutputSpec, bool, error) {
	spec := video.OutputSpec{Container: d.Container, Quality: d.Quality}
	explicit := false
	if ext != "" {
		c, ok := video.ParseContainer(strings.ToLower(ext))
		if !ok {
			return spec, false, video.ErrInvalidVideoID
		}
		spec.Container = c
		explicit = true
	}
	for _, seg := range rest {
		if seg == "" {
			continue
		}
		q, e := splitExt(seg)
		if e != "" {
			c, ok := video.ParseContainer(strings.ToLower(e))
			if !ok {
				return spec, false, video.ErrInvalidVideoID
			}
			spec.Container = c
			explicit = true
		}
		cap, err := video.ParseQuality(q)
		if err != nil {
			return spec, false, err
		}
		spec.Quality = cap
	}
	spec.Quality = spec.Quality.Clamp(d.MaxQuality)
	return spec, explicit, nil
}

// restoreSchemeSlash undoes net/http's path cleaning, which collapses
// "https://host" to "https:/host" before any handler sees the path.
// CRITICAL: without this, every pasted full URL fails to parse.
func restoreSchemeSlash(p string) string {
	for _, scheme := range []string{"https:", "http:"} {
		if len(p) > len(scheme) && strings.EqualFold(p[:len(scheme)], scheme) {
			rest := p[len(scheme):]
			if strings.HasPrefix(rest, "//") {
				return p
			}
			return p[:len(scheme)] + "//" + strings.TrimPrefix(rest, "/")
		}
	}
	return p
}

// splitExt separates a trailing .mp4 / .m3u8 from a segment. It only
// treats known media extensions as such, so an 11-character ID that
// happens to contain a dot is left alone.
func splitExt(seg string) (string, string) {
	i := strings.LastIndex(seg, ".")
	if i < 0 {
		return seg, ""
	}
	ext := seg[i+1:]
	if _, ok := video.ParseContainer(strings.ToLower(ext)); !ok {
		return seg, ""
	}
	return seg[:i], ext
}

// extractIDFromURL pulls the video ID out of a pasted YouTube URL.
func extractIDFromURL(raw string) (video.ID, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", video.ErrInvalidVideoID
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch host {
	case "youtu.be":
		return video.ParseID(strings.Trim(u.Path, "/"))
	case "youtube.com", "m.youtube.com", "music.youtube.com":
		if v := u.Query().Get("v"); v != "" {
			return video.ParseID(v)
		}
		// /embed/{id}, /shorts/{id}, /live/{id}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 {
			switch parts[0] {
			case "embed", "shorts", "live", "v":
				return video.ParseID(parts[1])
			}
		}
	}
	return "", video.ErrInvalidVideoID
}
