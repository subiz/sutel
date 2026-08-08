package audio

import (
	"errors"
	"math"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"
)

func TestMatchCases(t *testing.T) {
	expected := testSignal(5 * 8000)
	tests := []struct {
		name          string
		received      []int16
		minSimilarity float64
		minCoverage   float64
		maxCoverage   float64
	}{
		{name: "exact", received: expected, minSimilarity: 0.999, minCoverage: 0.99, maxCoverage: 1},
		{name: "leading-100ms", received: append(make([]int16, 800), expected...), minSimilarity: 0.99, minCoverage: 0.99, maxCoverage: 1},
		{name: "leading-500ms", received: append(make([]int16, 4000), expected...), minSimilarity: 0.99, minCoverage: 0.99, maxCoverage: 1},
		{name: "gain-minus-6db", received: scale(expected, 0.5), minSimilarity: 0.99, minCoverage: 0.99, maxCoverage: 1},
		{name: "first-80-percent", received: expected[:len(expected)*8/10], minSimilarity: 0.99, minCoverage: 0.76, maxCoverage: 0.84},
		{name: "first-50-percent", received: expected[:len(expected)/2], minSimilarity: 0.99, minCoverage: 0.46, maxCoverage: 0.54},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Match(expected, test.received, MatchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Similarity < test.minSimilarity {
				t.Fatalf("similarity=%.4f want >= %.4f", result.Similarity, test.minSimilarity)
			}
			if result.Coverage < test.minCoverage || result.Coverage > test.maxCoverage {
				t.Fatalf("coverage=%.4f want [%.4f,%.4f]", result.Coverage, test.minCoverage, test.maxCoverage)
			}
		})
	}
}

func TestMatchExtraPrefixSuffixAndPeriodicAlignment(t *testing.T) {
	expected := make([]int16, 3*8000)
	for index := range expected {
		seconds := float64(index) / 8000
		value := 12000*math.Sin(2*math.Pi*440*seconds) + 5000*math.Sin(2*math.Pi*613*seconds)
		// Flip phase without changing the energy envelope. Alignment that only
		// looks at energy cannot use this marker.
		if seconds >= 1.1 && seconds < 1.35 {
			value = -value
		}
		expected[index] = int16(value)
	}
	prefix := make([]int16, 370*time.Millisecond*8000/time.Second)
	for index := range prefix {
		seconds := float64(index) / 8000
		prefix[index] = int16(12000*math.Cos(2*math.Pi*440*seconds) + 5000*math.Cos(2*math.Pi*613*seconds))
	}
	received := append(prefix, expected...)
	received = append(received, testSignal(4000)...)
	result, err := Match(expected, received, MatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Similarity < 0.95 || result.Coverage < 0.95 || result.AlignmentOffset != 370*time.Millisecond {
		t.Fatalf("periodic alignment=%+v", result)
	}
}

func TestMatchNoComparableWindowsIsFiniteZero(t *testing.T) {
	result, err := Match(testSignal(8000), []int16{100, -100, 100}, MatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Similarity != 0 || result.Coverage != 0 || math.IsNaN(result.Similarity) || math.IsNaN(result.Coverage) {
		t.Fatalf("result=%+v", result)
	}
}

func TestMatchRejectsWrongAudio(t *testing.T) {
	expected := testSignal(3 * 8000)
	wrong := make([]int16, len(expected))
	for index := range wrong {
		wrong[index] = int16(rand.IntN(60000) - 30000)
	}
	result, err := Match(expected, wrong, MatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Similarity >= 0.5 || result.Coverage >= 0.2 {
		t.Fatalf("wrong audio unexpectedly matched: %+v", result)
	}
}

func TestMatchRejectsSilence(t *testing.T) {
	if _, err := Match(make([]int16, 8000), make([]int16, 8000), MatchOptions{}); !errors.Is(err, ErrNoActiveAudio) {
		t.Fatalf("got %v want ErrNoActiveAudio", err)
	}
	result, err := Match(testSignal(8000), make([]int16, 8000), MatchOptions{})
	if err == nil && (result.Similarity != 0 || result.Coverage != 0) {
		t.Fatalf("silence result=%+v", result)
	}
}

func TestMatchSilenceThresholdBoundary(t *testing.T) {
	threshold := math.Pow(10, -45.0/20) * 32768
	makeAlternating := func(amplitude int16) []int16 {
		result := make([]int16, 800)
		for index := range result {
			if index%2 == 0 {
				result[index] = amplitude
			} else {
				result[index] = -amplitude
			}
		}
		return result
	}
	below := makeAlternating(int16(math.Floor(threshold) - 1))
	if _, err := Match(below, below, MatchOptions{SilenceThresholdDBFS: -45}); !errors.Is(err, ErrNoActiveAudio) {
		t.Fatalf("below-threshold err=%v", err)
	}
	above := makeAlternating(int16(math.Ceil(threshold) + 1))
	result, err := Match(above, above, MatchOptions{SilenceThresholdDBFS: -45})
	if err != nil || result.Similarity < 0.99 || result.Coverage < 0.99 {
		t.Fatalf("above-threshold result=%+v err=%v", result, err)
	}
}

func TestProvidedSampleG711RoundTripMatch(t *testing.T) {
	samples, err := Normalize8kMono(filepath.Join("..", "testdata", "sample.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) > 4*8000 {
		samples = samples[:4*8000]
	}
	for _, codec := range []string{"PCMA", "PCMU"} {
		received := DecodeG711(EncodeG711(samples, codec), codec)
		result, err := Match(samples, received, MatchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Similarity < 0.95 || result.Coverage < 0.95 {
			t.Fatalf("%s sample match=%+v", codec, result)
		}
	}
}

func testSignal(length int) []int16 {
	result := make([]int16, length)
	for index := range result {
		t := float64(index) / 8000
		envelope := 0.35 + 0.65*math.Pow(math.Sin(math.Pi*t/0.73), 2)
		value := math.Sin(2*math.Pi*(170+23*t)*t) + 0.4*math.Sin(2*math.Pi*613*t) + 0.2*math.Sin(2*math.Pi*997*t)
		result[index] = int16(16000 * envelope * value / 1.6)
	}
	return result
}

func scale(samples []int16, gain float64) []int16 {
	result := make([]int16, len(samples))
	for index, sample := range samples {
		result[index] = int16(float64(sample) * gain)
	}
	return result
}
