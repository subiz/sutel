package sutel

import (
	"errors"
	"fmt"

	mediaaudio "github.com/subiz/sutel/audio"
)

// VerifyAudio compares WAV data captured by the SUT with the expected WAV
// fixture. It uses the same normalization, alignment, similarity, coverage,
// and threshold rules as ExpectAudio on a call scenario.
//
// The returned result is populated even when the thresholds are not met. In
// that case err matches ErrVerification. Invalid expected fixtures match
// ErrInvalidExpectation.
func VerifyAudio(expectation AudioExpectation, receivedWAV []byte) (AudioMatchResult, error) {
	if err := validateAudioExpectation("AudioExpectation", &expectation); err != nil {
		return AudioMatchResult{}, errors.Join(ErrInvalidExpectation, err)
	}
	if len(receivedWAV) == 0 {
		return AudioMatchResult{}, fmt.Errorf("received audio must not be empty")
	}
	decoded, err := mediaaudio.DecodeWAV(receivedWAV)
	if err != nil {
		return AudioMatchResult{}, fmt.Errorf("received audio: %w", err)
	}
	received := mediaaudio.Resample(decoded.Samples, decoded.SampleRate, 8000)
	return verifyAudioSamples(expectation, received)
}

func verifyAudioSamples(expectation AudioExpectation, received []int16) (AudioMatchResult, error) {
	expected, err := mediaaudio.Normalize8kMono(expectation.File)
	if err != nil {
		return AudioMatchResult{}, errors.Join(ErrInvalidExpectation, fmt.Errorf("expected audio fixture: %w", err))
	}
	match, err := mediaaudio.Match(expected, received, mediaaudio.MatchOptions{
		MaxAlignmentOffset:   expectation.Match.MaxAlignmentOffset,
		FrameDuration:        expectation.Match.FrameDuration,
		FrameMatchThreshold:  expectation.Match.FrameMatchThreshold,
		SilenceThresholdDBFS: expectation.Match.SilenceThresholdDBFS,
	})
	if err != nil {
		if errors.Is(err, mediaaudio.ErrNoActiveAudio) {
			return AudioMatchResult{}, errors.Join(ErrInvalidExpectation, fmt.Errorf("audio match: %w", err))
		}
		return AudioMatchResult{}, fmt.Errorf("audio match: %w", err)
	}
	result := AudioMatchResult{
		Similarity: match.Similarity, Coverage: match.Coverage, AlignmentOffset: match.AlignmentOffset,
		ExpectedDuration: match.ExpectedDuration, ReceivedDuration: match.ReceivedDuration, ComparedDuration: match.ComparedDuration,
	}
	if match.Similarity < expectation.MinSimilarity || match.Coverage < expectation.MinCoverage {
		return result, fmt.Errorf("%w: audio similarity %.3f/%.3f coverage %.3f/%.3f", ErrVerification, match.Similarity, expectation.MinSimilarity, match.Coverage, expectation.MinCoverage)
	}
	return result, nil
}
