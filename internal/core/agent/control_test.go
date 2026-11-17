package agent

import (
	"testing"

	"github.com/artemidius/insteadly/internal/core/domain"
)

func TestControlRouterDTMFEvidenceIsBoundedAndNotEchoed(t *testing.T) {
	control := make(chan domain.CallEvent, 32)
	r := newControlRouter(control)
	r.start()
	defer r.stopRouter()
	for i := 0; i < dtmfRingSize+5; i++ {
		control <- domain.CallEvent{Kind: domain.EventDTMF, DTMF: "1"}
	}
	control <- domain.CallEvent{Kind: domain.EventPlaybackMark, PlaybackID: "m1"}
	<-r.marksChannel()
	got := r.dtmfSnapshot()
	if len(got) != dtmfRingSize {
		t.Fatalf("DTMF ring length = %d, want bounded %d", len(got), dtmfRingSize)
	}
	close(control)
	<-r.doneChannel()
}
