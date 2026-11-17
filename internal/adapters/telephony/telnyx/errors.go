package telnyx

import (
	"errors"
)

// Adapter-local failures the core may see. They are deliberately not ports sentinels:
// they describe this transport, not a domain condition.
var (
	ErrSessionEnded    = errors.New("telnyx: call session ended")
	ErrSessionNotReady = errors.New("telnyx: call session not ready")
	ErrDTMFUnsupported = errors.New("telnyx: outbound dtmf is not supported")
)

// alreadyEndedCode is Telnyx's documented error code for a hangup on a finished call
// ("Call has already ended"). It is treated as success, exactly like a natural end.
const alreadyEndedCode = "90018"
