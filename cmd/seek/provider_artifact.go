package main

import "time"

// A compiled accelerator model is expensive to produce and is cached on disk.
// Two parts of Seek share these: the provider code records that a model is still
// wanted, and the collector ages models out on that record. Neither can own the
// declarations. The collector has no build tag and must compile on every target,
// while the provider code is built only where an accelerator exists.

// providerArtifactUsedFile records the last time a process wanted a compiled
// accelerator model. The collector ages on it, the way it ages a corpus on its
// own marker.
const providerArtifactUsedFile = ".used"

// providerArtifactTouchInterval bounds how often a search rewrites that marker.
const providerArtifactTouchInterval = 24 * time.Hour
