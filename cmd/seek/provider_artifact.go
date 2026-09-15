package main

// A compiled accelerator model is expensive to produce and is cached on disk.
// Two parts of Seek share this name: the provider code records that a model is
// still wanted, and the collector ages models out on that record. Neither can
// own the declaration. The collector has no build tag and must compile on every
// target, while the provider code is built only where an accelerator exists.
//
// Only a name both sides read belongs here. A constant this file declares but
// only an accelerated target uses is dead on every other target, where the
// unused linter then fails the build — while a build on an accelerated host
// stays green and hides it.

// providerArtifactUsedFile records the last time a process wanted a compiled
// accelerator model. The collector ages on it, the way it ages a corpus on its
// own marker.
const providerArtifactUsedFile = ".used"
