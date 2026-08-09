package sutel

import (
	"bytes"
	"errors"
	"os"
	"testing"

	mediaaudio "github.com/subiz/sutel/audio"
)

func TestVerifyAudioUsesCallExpectationPipeline(t *testing.T) {
	expectation := AudioExpectation{
		File: "testdata/sample.wav", MinSimilarity: 0.95, MinCoverage: 0.95,
	}
	receivedWAV, err := os.ReadFile("testdata/sample.wav")
	if err != nil {
		t.Fatal(err)
	}
	match, err := VerifyAudio(expectation, receivedWAV)
	if err != nil {
		t.Fatal(err)
	}
	if match.Similarity < 0.99 || match.Coverage < 0.99 || match.ComparedDuration <= 0 {
		t.Fatalf("match=%+v", match)
	}

	samples, err := mediaaudio.Normalize8kMono(expectation.File)
	if err != nil {
		t.Fatal(err)
	}
	var partial bytes.Buffer
	if err := mediaaudio.WriteWAV(&partial, samples[:len(samples)/4], 8000); err != nil {
		t.Fatal(err)
	}
	match, err = VerifyAudio(expectation, partial.Bytes())
	if !errors.Is(err, ErrVerification) {
		t.Fatalf("match=%+v err=%v", match, err)
	}
	if match.Coverage >= expectation.MinCoverage {
		t.Fatalf("coverage=%f want below %f", match.Coverage, expectation.MinCoverage)
	}
}

// Sutel itself never negotiates Opus, but a SUT talking Opus to a real
// carrier decodes that stream with its own codec and hands the captured PCM
// WAV to VerifyAudio. The fixture is testdata/sample.wav through an Opus
// 12 kbps VoIP round trip plus a 186-sample off-grid constant delay, the
// shape of a real SUT-side capture (codec quantization + decoder lookahead).
func TestVerifyAudioMatchesOpusDegradedCapture(t *testing.T) {
	receivedWAV, err := os.ReadFile("testdata/opus_degraded.wav")
	if err != nil {
		t.Fatal(err)
	}
	match, err := VerifyAudio(AudioExpectation{
		File: "testdata/sample.wav", MinSimilarity: 0.90, MinCoverage: 0.95,
	}, receivedWAV)
	if err != nil {
		t.Fatalf("match=%+v err=%v", match, err)
	}
	if match.Similarity < 0.90 || match.Coverage < 0.95 {
		t.Fatalf("opus-degraded capture did not verify: %+v", match)
	}
}

func TestVerifyAudioClassifiesInvalidExpectedFixture(t *testing.T) {
	_, err := VerifyAudio(AudioExpectation{File: "testdata/missing.wav"}, []byte("not used"))
	if !errors.Is(err, ErrInvalidExpectation) {
		t.Fatalf("err=%v", err)
	}
}

func TestVerifyAudioRejectsMalformedReceivedWAV(t *testing.T) {
	_, err := VerifyAudio(AudioExpectation{File: "testdata/sample.wav"}, []byte("not a WAV"))
	if err == nil || errors.Is(err, ErrInvalidExpectation) || errors.Is(err, ErrVerification) {
		t.Fatalf("err=%v", err)
	}
}
