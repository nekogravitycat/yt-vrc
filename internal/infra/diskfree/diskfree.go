// Package diskfree reports free space on the volume holding a path.
//
// NOTE: tracked as a health metric (spec §4.6) because a full disk fails
// silently -- ffmpeg writes a truncated artifact, the store still
// publishes it, and playback just stops early.
package diskfree
