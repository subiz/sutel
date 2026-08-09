package sutel

import (
	"fmt"
	"math"
	"net/netip"
	"os"
	"strings"
	"time"
)

type OutboundBehavior interface {
	behaviorName() string
}

type Answer struct {
	OmitTrying bool

	TryingAfter  time.Duration
	RingingAfter time.Duration
	AnswerAfter  time.Duration
	HangupAfter  time.Duration

	Playback *AudioPlayback
	Echo     *AudioEcho
}

func (Answer) behaviorName() string { return "answer" }

type Busy struct{}
type Reject struct{}
type NotFound struct{}
type ServiceUnavailable struct{}
type NoAnswer struct{}
type Timeout struct{}

// NetworkLoss establishes the call, then makes the Sutel endpoint disappear
// silently without sending BYE.
type NetworkLoss struct {
	// After is measured from the matching ACK. Zero drops immediately.
	After time.Duration
}

func (Busy) behaviorName() string               { return "busy" }
func (Reject) behaviorName() string             { return "reject" }
func (NotFound) behaviorName() string           { return "not-found" }
func (ServiceUnavailable) behaviorName() string { return "service-unavailable" }
func (NoAnswer) behaviorName() string           { return "no-answer" }
func (Timeout) behaviorName() string            { return "timeout" }
func (NetworkLoss) behaviorName() string        { return "network-loss" }

type EarlyMedia struct {
	File          string
	ProgressAfter time.Duration
	SendRinging   bool
	RingingAfter  time.Duration
	AnswerAfter   time.Duration
	HangupAfter   time.Duration
}

func (EarlyMedia) behaviorName() string { return "early-media" }

// EarlyFailure sends provisional early media and then completes the INVITE
// with a non-2xx final response. Timings are relative to the first INVITE.
type EarlyFailure struct {
	File string

	OmitTrying    bool
	TryingAfter   time.Duration
	ProgressAfter time.Duration
	FailureAfter  time.Duration

	// FinalStatus defaults to 486.
	FinalStatus int
	// FinalReason overrides the SIP status reason phrase when non-empty.
	FinalReason string
	// ReasonHeader is an optional SIP Reason header, for example
	// `Q.850;cause=17;text="USER_BUSY"`.
	ReasonHeader string
}

func (EarlyFailure) behaviorName() string { return "early-failure" }

type DTMFMethod uint8

const (
	RFC4733 DTMFMethod = iota
	SIPInfo
)

type DTMFExpectation struct {
	Method DTMFMethod
	Digits string
}

type DTMFAction struct {
	Method   DTMFMethod
	Digits   string
	Interval time.Duration
}

type AudioExpectation struct {
	File string

	MinSimilarity float64
	MinCoverage   float64

	Match AudioMatchOptions
}

type AudioMatchOptions struct {
	// Alignment is evaluated on a deterministic 10 ms grid.
	MaxAlignmentOffset   time.Duration
	FrameDuration        time.Duration
	FrameMatchThreshold  float64
	SilenceThresholdDBFS float64
}

type AudioPlayback struct {
	File string
}

type AudioEcho struct {
	Delay time.Duration
}

type OutboundScenario struct {
	LocalIP string
	// LocalPort is the SIP UDP port. Zero lets the OS select a free port.
	LocalPort uint16
	Timeout   time.Duration

	// From matches the user part of the From URI exactly. To matches the user
	// parts of both the To URI and Request-URI. An empty value skips the check.
	From string
	To   string
	// FromDisplayName matches the display name in the From header exactly. An
	// empty value skips the check.
	FromDisplayName string

	Behavior OutboundBehavior
	Codecs   []Codec

	DTMF        *DTMFExpectation
	ExpectAudio *AudioExpectation
}

func (s OutboundScenario) validate() error {
	if s.Behavior == nil {
		s.Behavior = Answer{}
	}
	if s.Timeout < 0 {
		return fmt.Errorf("Timeout must not be negative")
	}
	if err := validateCodecs(s.Codecs); err != nil {
		return err
	}
	if err := validateAudioExpectation("ExpectAudio", s.ExpectAudio); err != nil {
		return err
	}
	if s.DTMF != nil {
		if err := validateDTMFDigits(s.DTMF.Digits); err != nil {
			return fmt.Errorf("DTMF: %w", err)
		}
	}
	switch behavior := s.Behavior.(type) {
	case Answer:
		if err := validateAnswerTimings(behavior); err != nil {
			return err
		}
		if behavior.Playback != nil && behavior.Echo != nil {
			return fmt.Errorf("Answer.Playback and Answer.Echo cannot both be set")
		}
		if behavior.Playback != nil {
			if behavior.Playback.File == "" {
				return fmt.Errorf("Answer.Playback.File must not be empty")
			}
			if _, err := os.Stat(behavior.Playback.File); err != nil {
				return fmt.Errorf("Answer.Playback.File: %w", err)
			}
		}
		if behavior.Echo != nil && behavior.Echo.Delay < 0 {
			return fmt.Errorf("Answer.Echo.Delay must not be negative")
		}
		return nil
	case EarlyMedia:
		if behavior.File == "" {
			return fmt.Errorf("EarlyMedia.File must not be empty")
		}
		if _, err := os.Stat(behavior.File); err != nil {
			return fmt.Errorf("EarlyMedia.File: %w", err)
		}
		if behavior.ProgressAfter < 0 || behavior.RingingAfter < 0 || behavior.AnswerAfter < 0 || behavior.HangupAfter < 0 {
			return fmt.Errorf("early-media timings must not be negative")
		}
		if behavior.AnswerAfter < behavior.ProgressAfter || behavior.SendRinging && (behavior.RingingAfter < behavior.ProgressAfter || behavior.AnswerAfter < behavior.RingingAfter) {
			return fmt.Errorf("early-media timings must be nondecreasing")
		}
		if behavior.HangupAfter > 0 && behavior.HangupAfter < behavior.AnswerAfter {
			return fmt.Errorf("HangupAfter must not precede AnswerAfter")
		}
	case EarlyFailure:
		if behavior.File == "" {
			return fmt.Errorf("EarlyFailure.File must not be empty")
		}
		if _, err := os.Stat(behavior.File); err != nil {
			return fmt.Errorf("EarlyFailure.File: %w", err)
		}
		if behavior.TryingAfter < 0 || behavior.ProgressAfter < 0 || behavior.FailureAfter < 0 {
			return fmt.Errorf("early-failure timings must not be negative")
		}
		if (!behavior.OmitTrying && behavior.ProgressAfter < behavior.TryingAfter) || behavior.FailureAfter < behavior.ProgressAfter {
			return fmt.Errorf("early-failure timings must be nondecreasing")
		}
		if behavior.FinalStatus != 0 && (behavior.FinalStatus < 400 || behavior.FinalStatus > 699) {
			return fmt.Errorf("EarlyFailure.FinalStatus must be in [400,699]")
		}
		if strings.ContainsAny(behavior.FinalReason, "\r\n") || strings.ContainsAny(behavior.ReasonHeader, "\r\n") {
			return fmt.Errorf("early-failure response values must not contain CR or LF")
		}
	case NetworkLoss:
		if behavior.After < 0 {
			return fmt.Errorf("NetworkLoss.After must not be negative")
		}
		return nil
	case Busy, Reject, NotFound, ServiceUnavailable, NoAnswer, Timeout:
		return nil
	default:
		return fmt.Errorf("unsupported outbound behavior %T", behavior)
	}
	return nil
}

type InboundScenario struct {
	LocalIP string
	// LocalPort is the SIP UDP port. Zero lets the OS select a free port.
	LocalPort uint16
	Timeout   time.Duration

	TargetSIPAddr netip.AddrPort

	From string
	To   string
	// FromDisplayName is emitted in the caller's SIP From header.
	FromDisplayName string

	// DigestCredentials makes the simulated caller retry an INVITE challenged
	// with 401 using RFC 7616 Digest authentication. Leave nil for carrier
	// trunks that authenticate by source IP.
	DigestCredentials *DigestCredentials

	Codec Codec

	Playback    *AudioPlayback
	ExpectAudio *AudioExpectation
	DTMF        []DTMFAction

	// ExpectStatus defaults to 200 and must be a final INVITE status.
	ExpectStatus int

	RingTimeout  time.Duration
	CallDuration time.Duration
}

type DigestCredentials struct {
	Username string
	Password string
}

func (s InboundScenario) validate() error {
	if !s.TargetSIPAddr.IsValid() {
		return fmt.Errorf("TargetSIPAddr is required")
	}
	if s.From == "" || s.To == "" || strings.ContainsAny(s.From+s.To, "\r\n<>@; ") {
		return fmt.Errorf("From and To are required")
	}
	if strings.ContainsAny(s.FromDisplayName, "\r\n") {
		return fmt.Errorf("FromDisplayName contains CR or LF")
	}
	if s.DigestCredentials != nil {
		if s.DigestCredentials.Username == "" || strings.ContainsAny(s.DigestCredentials.Username, "\r\n\"") ||
			strings.ContainsAny(s.DigestCredentials.Password, "\r\n") {
			return fmt.Errorf("DigestCredentials contains invalid username or password")
		}
		if s.ExpectStatus == 401 {
			return fmt.Errorf("ExpectStatus 401 conflicts with DigestCredentials: the 401 is consumed by the authentication retry")
		}
	}
	if s.Codec != PCMA && s.Codec != PCMU {
		return fmt.Errorf("unsupported codec %d", s.Codec)
	}
	if s.Timeout < 0 || s.RingTimeout < 0 || s.CallDuration < 0 {
		return fmt.Errorf("call timings must not be negative")
	}
	if s.ExpectStatus != 0 && (s.ExpectStatus < 200 || s.ExpectStatus > 699) {
		return fmt.Errorf("ExpectStatus must be a final SIP status in [200,699]")
	}
	if s.Playback != nil {
		if s.Playback.File == "" {
			return fmt.Errorf("Playback.File must not be empty")
		}
		if _, err := os.Stat(s.Playback.File); err != nil {
			return fmt.Errorf("Playback.File: %w", err)
		}
	}
	if err := validateAudioExpectation("ExpectAudio", s.ExpectAudio); err != nil {
		return err
	}
	for index, action := range s.DTMF {
		if err := validateDTMFDigits(action.Digits); err != nil {
			return fmt.Errorf("DTMF[%d]: %w", index, err)
		}
		if action.Interval < 0 {
			return fmt.Errorf("DTMF[%d].Interval must not be negative", index)
		}
	}
	return nil
}

func validateAudioExpectation(name string, expectation *AudioExpectation) error {
	if expectation == nil {
		return nil
	}
	if expectation.File == "" {
		return fmt.Errorf("%s.File must not be empty", name)
	}
	if _, err := os.Stat(expectation.File); err != nil {
		return fmt.Errorf("%s.File: %w", name, err)
	}
	if math.IsNaN(expectation.MinSimilarity) || math.IsNaN(expectation.MinCoverage) ||
		math.IsInf(expectation.MinSimilarity, 0) || math.IsInf(expectation.MinCoverage, 0) ||
		expectation.MinSimilarity < 0 || expectation.MinSimilarity > 1 ||
		expectation.MinCoverage < 0 || expectation.MinCoverage > 1 {
		return fmt.Errorf("%s thresholds must be in [0,1]", name)
	}
	options := expectation.Match
	if options.MaxAlignmentOffset < 0 || options.FrameDuration < 0 ||
		options.FrameMatchThreshold < 0 || options.FrameMatchThreshold > 1 ||
		options.SilenceThresholdDBFS < -96 || options.SilenceThresholdDBFS > 0 ||
		math.IsNaN(options.FrameMatchThreshold) || math.IsNaN(options.SilenceThresholdDBFS) ||
		math.IsInf(options.FrameMatchThreshold, 0) || math.IsInf(options.SilenceThresholdDBFS, 0) {
		return fmt.Errorf("invalid %s match options", name)
	}
	return nil
}

func validateCodecs(codecs []Codec) error {
	seen := make(map[Codec]bool)
	for _, codec := range codecs {
		if codec != PCMA && codec != PCMU {
			return fmt.Errorf("unsupported codec %d", codec)
		}
		if seen[codec] {
			return fmt.Errorf("duplicate codec %s", codec)
		}
		seen[codec] = true
	}
	return nil
}

func validateAnswerTimings(behavior Answer) error {
	if behavior.TryingAfter < 0 || behavior.RingingAfter < 0 || behavior.AnswerAfter < 0 || behavior.HangupAfter < 0 {
		return fmt.Errorf("answer timings must not be negative")
	}
	if behavior.AnswerAfter < behavior.RingingAfter || !behavior.OmitTrying && behavior.RingingAfter < behavior.TryingAfter {
		return fmt.Errorf("answer timings must be nondecreasing")
	}
	if behavior.HangupAfter > 0 && behavior.HangupAfter < behavior.AnswerAfter {
		return fmt.Errorf("HangupAfter must not precede AnswerAfter")
	}
	return nil
}

func validateDTMFDigits(digits string) error {
	if digits == "" {
		return fmt.Errorf("digits must not be empty")
	}
	for _, digit := range digits {
		if !(digit >= '0' && digit <= '9') && digit != '*' && digit != '#' && !(digit >= 'A' && digit <= 'D') && !(digit >= 'a' && digit <= 'd') {
			return fmt.Errorf("invalid digit %q", digit)
		}
	}
	return nil
}
