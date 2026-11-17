package recorder

import "errors"

// errNoSTT is returned internally when a transcript was requested but no batch
// transcription adapter was wired. The composition root always wires one; a
// hand-built recorder in a test may not.
var errNoSTT = errors.New("recorder: no speech-to-text adapter configured")
