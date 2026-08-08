package sutel

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/subiz/sutel/internal/sip"
)

func TestRuntimeConfigDefaults(t *testing.T) {
	config, err := newRuntimeConfig("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if config.LocalIP != "127.0.0.1" || config.SIPT1 != 500*time.Millisecond || config.SIPT2 != 4*time.Second || config.RTPReorderWindow != 10 || config.MaxSIPMessageBytes != 64*1024 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
}

func TestRuntimeConfigRejectsInvalidTimers(t *testing.T) {
	config, err := newRuntimeConfig("", 0)
	if err != nil {
		t.Fatal(err)
	}
	config.SIPT1 = time.Second
	config.SIPT2 = 500 * time.Millisecond
	if err := config.validate(); err == nil {
		t.Fatal("accepted SIPT2 below SIPT1")
	}
}

func TestRuntimeConfigRejectsUnsafeBind(t *testing.T) {
	if _, err := newRuntimeConfig("0.0.0.0", 0); err == nil {
		t.Fatal("accepted unspecified LocalIP")
	}
}

func TestAnswerTimingValidation(t *testing.T) {
	if err := (OutboundScenario{Behavior: Answer{OmitTrying: true, TryingAfter: time.Second, RingingAfter: 10 * time.Millisecond, AnswerAfter: 20 * time.Millisecond}, Codecs: []Codec{PCMA}}).validate(); err != nil {
		t.Fatalf("disabled Trying affected timing validation: %v", err)
	}
	if err := (OutboundScenario{Behavior: Answer{AnswerAfter: time.Second, HangupAfter: 500 * time.Millisecond}, Codecs: []Codec{PCMA}}).validate(); err == nil {
		t.Fatal("accepted HangupAfter before AnswerAfter")
	}
}

func TestAnswerRejectsPlaybackAndEchoTogether(t *testing.T) {
	scenario := OutboundScenario{Behavior: Answer{
		Playback: &AudioPlayback{File: "testdata/sample.wav"},
		Echo:     &AudioEcho{Delay: time.Second},
	}}
	if err := scenario.validate(); err == nil {
		t.Fatal("accepted Answer with both Playback and Echo")
	}
}

func TestNetworkLossRejectsNegativeDelay(t *testing.T) {
	if err := (OutboundScenario{Behavior: NetworkLoss{After: -time.Millisecond}}).validate(); err == nil {
		t.Fatal("accepted negative NetworkLoss.After")
	}
}

func TestEarlyFailureValidation(t *testing.T) {
	valid := OutboundScenario{
		Behavior: EarlyFailure{File: "testdata/sample.wav", ProgressAfter: 10 * time.Millisecond, FailureAfter: 20 * time.Millisecond},
		Codecs:   []Codec{PCMA},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid EarlyFailure: %v", err)
	}
	invalidStatus := valid
	invalidStatus.Behavior = EarlyFailure{File: "testdata/sample.wav", FinalStatus: 200}
	if err := invalidStatus.validate(); err == nil {
		t.Fatal("accepted 2xx EarlyFailure status")
	}
	invalidTiming := valid
	invalidTiming.Behavior = EarlyFailure{File: "testdata/sample.wav", ProgressAfter: time.Second, FailureAfter: time.Millisecond}
	if err := invalidTiming.validate(); err == nil {
		t.Fatal("accepted failure before 183")
	}
	injection := valid
	injection.Behavior = EarlyFailure{File: "testdata/sample.wav", ReasonHeader: "Q.850;cause=17\r\nX-Bad: true"}
	if err := injection.validate(); err == nil {
		t.Fatal("accepted Reason header injection")
	}
}

func TestCallResultAccessorsReturnCopies(t *testing.T) {
	original := CallResult{
		dtmf: []DTMFEvent{{Digit: "5"}}, receivedPCM: []int16{1, 2},
		events: []Event{{Type: "INVITE"}}, Audio: &AudioMatchResult{Similarity: 1},
	}
	clone := original.clone()
	clone.dtmf[0].Digit = "9"
	clone.receivedPCM[0] = 9
	clone.events[0].Type = "BYE"
	clone.Audio.Similarity = 0
	if original.dtmf[0].Digit != "5" || original.receivedPCM[0] != 1 || original.events[0].Type != "INVITE" || original.Audio.Similarity != 1 {
		t.Fatal("clone mutated original result")
	}
}

func TestDialogLooseAndStrictRouting(t *testing.T) {
	loose := &callDialog{
		peer: netip.MustParseAddrPort("127.0.0.1:5060"), remoteTarget: "sip:peer@127.0.0.1:5060",
		routeSet: []string{"<sip:proxy@127.0.0.1:5070;lr>", "<sip:edge@127.0.0.1:5080;lr>"},
	}
	request := sip.NewRequest("BYE", loose.remoteTarget)
	target, err := routeDialogRequest(request, loose)
	if err != nil || target.String() != "127.0.0.1:5070" || request.URI != loose.remoteTarget || len(request.Values("Route")) != 2 {
		t.Fatalf("loose target=%s request=%+v err=%v", target, request, err)
	}
	strict := &callDialog{
		peer: netip.MustParseAddrPort("127.0.0.1:5060"), remoteTarget: "sip:peer@127.0.0.1:5060",
		routeSet: []string{"<sip:proxy@127.0.0.1:5070>"},
	}
	request = sip.NewRequest("INFO", strict.remoteTarget)
	target, err = routeDialogRequest(request, strict)
	if err != nil || request.URI != "sip:proxy@127.0.0.1:5070" || len(request.Values("Route")) != 1 || request.Get("Route") != "<sip:peer@127.0.0.1:5060>" {
		t.Fatalf("strict target=%s request=%+v err=%v", target, request, err)
	}
}

func TestStructuredErrorSupportsErrorsIs(t *testing.T) {
	err := &Error{Kind: ErrVerification, Cause: errors.New("detail")}
	if !errors.Is(err, ErrVerification) {
		t.Fatal("errors.Is did not find ErrVerification")
	}
}

func TestStructuredErrorIncludesDetailFromSameKindCause(t *testing.T) {
	err := &Error{Kind: ErrVerification, Cause: fmt.Errorf("%w: got 999, want 100", ErrVerification)}
	if got := err.Error(); got != "sutel: call verification failed: got 999, want 100" {
		t.Fatalf("Error()=%q", got)
	}
}

func TestDiagnosticsIncludeProtocolMediaAndExpectationContext(t *testing.T) {
	session := &Session{outbound: &OutboundScenario{
		Behavior: Answer{}, ExpectAudio: &AudioExpectation{File: "greeting.wav", MinSimilarity: 0.95, MinCoverage: 0.80},
	}}
	result := CallResult{
		SIP:     SIPStats{LocalAddr: netip.MustParseAddrPort("127.0.0.1:5060"), RemoteAddr: netip.MustParseAddrPort("127.0.0.1:5070"), RequestsReceived: 2, ResponsesSent: 3, Retransmissions: 1},
		Outcome: SIPOutcome{InviteFinalStatus: 200, Established: true},
		Media:   NegotiatedMedia{AudioCodec: PCMA, TelephoneEventPayloadType: 101},
		RTP:     RTPStats{PacketsReceived: 10, PacketsSent: 5, ReceivedDuration: 200 * time.Millisecond},
		Audio:   &AudioMatchResult{Similarity: 0.90, Coverage: 0.70, ExpectedDuration: time.Second, ReceivedDuration: 900 * time.Millisecond},
		dtmf:    []DTMFEvent{{Digit: "5"}}, events: []Event{{Layer: SIPLayer, Direction: Received, Detail: "INVITE"}, {Layer: SIPLayer, Direction: Sent, Detail: "200 OK"}},
	}
	detail := diagnostics(session, result)
	for _, required := range []string{"scenario: outbound-answer", "state: final=200", "local=127.0.0.1:5060", "SIP <- INVITE", "codec=PCMA", `digits="5"`, `expected="greeting.wav"`, "similarity=0.900 required=0.950"} {
		if !strings.Contains(detail, required) {
			t.Fatalf("diagnostics missing %q:\n%s", required, detail)
		}
	}
}

func TestDTMFVerificationDoesNotDependOnCollectionOrder(t *testing.T) {
	session := &Session{outbound: &OutboundScenario{Behavior: Answer{}, DTMF: &DTMFExpectation{Method: RFC4733, Digits: "5"}}}
	result := CallResult{dtmfRecords: []dtmfRecord{
		{event: DTMFEvent{Digit: "5"}, method: SIPInfo, at: time.Unix(1, 0)},
		{event: DTMFEvent{Digit: "5"}, method: RFC4733, at: time.Unix(2, 0)},
	}}
	if err := session.verify(&result); !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), "unexpected DTMF method") {
		t.Fatalf("verify err=%v", err)
	}
}

func TestParseDTMFInfoRejectsDuplicateEmptyField(t *testing.T) {
	message := sip.NewRequest("INFO", "sip:127.0.0.1")
	message.Add("Content-Type", "application/dtmf-relay")
	message.Body = []byte("Signal=\r\nSignal=5\r\nDuration=160\r\n")
	if _, _, status := parseDTMFInfo(message); status != 400 {
		t.Fatalf("status=%d want 400", status)
	}
}

func TestChooseCodecUsesDynamicRTPMap(t *testing.T) {
	description := sip.SDP{Formats: []uint8{96}, RTPMaps: map[uint8]sip.RTPMap{96: {Encoding: "PCMA", ClockRate: 8000}}}
	codec, payload, ok := chooseCodec(description, []Codec{PCMA})
	if !ok || codec != PCMA || payload != 96 {
		t.Fatalf("codec=%v payload=%d ok=%t", codec, payload, ok)
	}
}

func TestSilenceOnlyFixtureIsInvalidExpectationNotVerification(t *testing.T) {
	path := writeShortWAV(t, make([]int16, 800))
	session := &Session{outbound: &OutboundScenario{Behavior: Answer{}, ExpectAudio: &AudioExpectation{File: path}}}
	err := session.verify(&CallResult{})
	if !errors.Is(err, ErrInvalidExpectation) || classifyError(err) != ErrInvalidExpectation {
		t.Fatalf("err=%v kind=%v", err, classifyError(err))
	}
}
