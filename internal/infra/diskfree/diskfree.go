// Package diskfree reports free space on the volume holding a path.
//
// spec §4.6 lists it as a health metric because this service's failure
// mode under a full disk is silent: ffmpeg writes a truncated artifact,
// the store publishes it, and the player shows a video that stops early.
// Knowing beforehand is the only cheap way to catch that.
package diskfree
