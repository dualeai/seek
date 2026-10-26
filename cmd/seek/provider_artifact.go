package main

// A compiled accelerator model is slow to make, so Seek keeps it on disk. Two
// parts of Seek share this name. The provider code records that a model is
// still wanted, and the collector removes old models by that record. Neither
// can own the declaration. The collector has no build tag and must compile on
// every target, while the provider code is built only where an accelerator
// exists.
//
// Only a name that both sides read belongs here. A constant here that only an
// accelerated target uses is unused on every other target. There the unused
// linter fails the build, while a build on an accelerated host passes and
// hides the defect.

// providerArtifactUsedFile records the last time a process wanted a compiled
// accelerator model. The collector measures the age of the model from it, as
// it measures the age of a corpus from usedFile.
const providerArtifactUsedFile = ".used"
