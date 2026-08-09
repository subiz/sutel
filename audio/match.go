package audio

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrNoActiveAudio = errors.New("no active audio")

type MatchOptions struct {
	MaxAlignmentOffset   time.Duration
	FrameDuration        time.Duration
	FrameMatchThreshold  float64
	SilenceThresholdDBFS float64
}

type MatchResult struct {
	Similarity       float64
	Coverage         float64
	AlignmentOffset  time.Duration
	ExpectedDuration time.Duration
	ReceivedDuration time.Duration
	ComparedDuration time.Duration
}

func (o MatchOptions) defaults() (MatchOptions, error) {
	if o.MaxAlignmentOffset == 0 {
		o.MaxAlignmentOffset = 2 * time.Second
	}
	if o.FrameDuration == 0 {
		o.FrameDuration = 100 * time.Millisecond
	}
	if o.FrameMatchThreshold == 0 {
		o.FrameMatchThreshold = 0.80
	}
	if o.SilenceThresholdDBFS == 0 {
		o.SilenceThresholdDBFS = -45
	}
	if o.MaxAlignmentOffset < 0 || o.FrameDuration <= 0 || o.FrameMatchThreshold <= 0 || o.FrameMatchThreshold > 1 || o.SilenceThresholdDBFS < -96 || o.SilenceThresholdDBFS >= 0 {
		return MatchOptions{}, fmt.Errorf("invalid audio match options")
	}
	return o, nil
}

func Match(expected, received []int16, options MatchOptions) (MatchResult, error) {
	options, err := options.defaults()
	if err != nil {
		return MatchResult{}, err
	}
	result := MatchResult{
		ExpectedDuration: samplesDuration(len(expected)),
		ReceivedDuration: samplesDuration(len(received)),
	}
	if len(expected) == 0 {
		return result, fmt.Errorf("expected audio: %w", ErrNoActiveAudio)
	}
	expectedFloat := normalize(expected)
	receivedFloat := normalize(received)
	if rms(expectedFloat) == 0 {
		return result, fmt.Errorf("expected audio: %w", ErrNoActiveAudio)
	}
	if len(receivedFloat) == 0 || rms(receivedFloat) == 0 {
		return result, nil
	}
	offset, err := align(expectedFloat, receivedFloat, options.MaxAlignmentOffset)
	if err != nil {
		if errors.Is(err, ErrNoActiveAudio) {
			return result, nil
		}
		return result, err
	}
	result.AlignmentOffset = samplesDuration(offset)
	frameSamples := durationSamples(options.FrameDuration)
	if frameSamples < 1 {
		frameSamples = 1
	}
	silenceRMS := math.Pow(10, options.SilenceThresholdDBFS/20)
	active := 0
	comparable := 0
	matched := 0
	scoreSum := 0.0
	comparedSamples := 0
	for start := 0; start < len(expectedFloat); start += frameSamples {
		end := min(start+frameSamples, len(expectedFloat))
		if end-start < frameSamples/2 {
			break
		}
		expectedFrame := expectedFloat[start:end]
		if rms(expectedFrame) <= silenceRMS {
			continue
		}
		active++
		receivedStart := start + offset
		receivedEnd := receivedStart + len(expectedFrame)
		if receivedStart < 0 || receivedEnd > len(receivedFloat) {
			continue
		}
		comparable++
		comparedSamples += len(expectedFrame)
		score := normalizedCorrelation(expectedFrame, receivedFloat[receivedStart:receivedEnd])
		if !isFinite(score) || score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		scoreSum += score
		if score >= options.FrameMatchThreshold {
			matched++
		}
	}
	if active == 0 {
		return result, fmt.Errorf("expected audio: %w", ErrNoActiveAudio)
	}
	result.Coverage = float64(matched) / float64(active)
	if comparable > 0 {
		result.Similarity = scoreSum / float64(comparable)
	}
	result.ComparedDuration = samplesDuration(comparedSamples)
	return result, nil
}

func normalize(samples []int16) []float64 {
	result := make([]float64, len(samples))
	if len(samples) == 0 {
		return result
	}
	mean := 0.0
	for _, sample := range samples {
		mean += float64(sample) / 32768
	}
	mean /= float64(len(samples))
	for index, sample := range samples {
		result[index] = float64(sample)/32768 - mean
	}
	return result
}

func align(expected, received []float64, maximum time.Duration) (int, error) {
	if len(received) == 0 || rms(expected) == 0 || rms(received) == 0 {
		return 0, ErrNoActiveAudio
	}
	window := 160
	hop := 80
	expectedEnvelope := energyEnvelope(expected, window, hop)
	receivedEnvelope := energyEnvelope(received, window, hop)
	maxHops := durationSamples(maximum) / hop
	bestOffset := 0
	bestScore := math.Inf(-1)
	for offset := -maxHops; offset <= maxHops; offset++ {
		var left, right []float64
		if offset >= 0 {
			if offset >= len(receivedEnvelope) {
				continue
			}
			length := min(len(expectedEnvelope), len(receivedEnvelope)-offset)
			left = expectedEnvelope[:length]
			right = receivedEnvelope[offset : offset+length]
		} else {
			start := -offset
			if start >= len(expectedEnvelope) {
				continue
			}
			length := min(len(expectedEnvelope)-start, len(receivedEnvelope))
			left = expectedEnvelope[start : start+length]
			right = receivedEnvelope[:length]
		}
		if len(left) < 3 {
			continue
		}
		energyScore := normalizedCorrelation(left, right)
		contentScore := sampledCorrelation(expected, received, offset*hop, 8)
		if energyScore < 0 {
			energyScore = 0
		}
		if contentScore < 0 {
			contentScore = 0
		}
		sampleOffset := offset * hop
		expectedStart, receivedStart := 0, 0
		if sampleOffset >= 0 {
			receivedStart = sampleOffset
		} else {
			expectedStart = -sampleOffset
		}
		overlap := min(len(expected)-expectedStart, len(received)-receivedStart)
		overlapRatio := float64(max(0, overlap)) / float64(min(len(expected), len(received)))
		score := (0.35*energyScore + 0.65*contentScore) * (0.25 + 0.75*overlapRatio)
		if score > bestScore+1e-12 || math.Abs(score-bestScore) <= 1e-12 && (abs(offset) < abs(bestOffset) || abs(offset) == abs(bestOffset) && offset < bestOffset) {
			bestScore = score
			bestOffset = offset
		}
	}
	if math.IsInf(bestScore, -1) || math.IsNaN(bestScore) {
		return 0, ErrNoActiveAudio
	}
	return refineAlignment(expected, received, bestOffset*hop, hop), nil
}

// refineAlignment searches one hop around the coarse grid offset at
// one-sample resolution. A constant sub-grid delay — for example the ~6.5 ms
// Opus codec lookahead — otherwise destroys waveform correlation even though
// the audio is correct. Scoring is unchanged; this only aligns better.
func refineAlignment(expected, received []float64, coarse, hop int) int {
	bestOffset := coarse
	bestScore := math.Inf(-1)
	for delta := -hop; delta <= hop; delta++ {
		offset := coarse + delta
		score := sampledCorrelation(expected, received, offset, 1)
		better := score > bestScore+1e-12
		tie := math.Abs(score-bestScore) <= 1e-12 &&
			(abs(offset-coarse) < abs(bestOffset-coarse) ||
				abs(offset-coarse) == abs(bestOffset-coarse) && offset < bestOffset)
		if better || tie {
			bestScore = score
			bestOffset = offset
		}
	}
	return bestOffset
}

func sampledCorrelation(expected, received []float64, offset, stride int) float64 {
	expectedStart, receivedStart := 0, 0
	if offset >= 0 {
		receivedStart = offset
	} else {
		expectedStart = -offset
	}
	length := min(len(expected)-expectedStart, len(received)-receivedStart)
	if length <= 0 {
		return 0
	}
	leftMean, rightMean, count := 0.0, 0.0, 0
	for index := 0; index < length; index += stride {
		leftMean += expected[expectedStart+index]
		rightMean += received[receivedStart+index]
		count++
	}
	if count < 3 {
		return 0
	}
	leftMean /= float64(count)
	rightMean /= float64(count)
	numerator, leftPower, rightPower := 0.0, 0.0, 0.0
	for index := 0; index < length; index += stride {
		left := expected[expectedStart+index] - leftMean
		right := received[receivedStart+index] - rightMean
		numerator += left * right
		leftPower += left * left
		rightPower += right * right
	}
	if leftPower == 0 || rightPower == 0 {
		return 0
	}
	return numerator / math.Sqrt(leftPower*rightPower)
}

func energyEnvelope(samples []float64, window, hop int) []float64 {
	if len(samples) == 0 {
		return nil
	}
	result := make([]float64, 0, (len(samples)+hop-1)/hop)
	for start := 0; start < len(samples); start += hop {
		end := min(start+window, len(samples))
		result = append(result, rms(samples[start:end]))
	}
	return result
}

func rms(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	value := 0.0
	for _, sample := range samples {
		value += sample * sample
	}
	return math.Sqrt(value / float64(len(samples)))
}

func normalizedCorrelation(left, right []float64) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	leftMean, rightMean := 0.0, 0.0
	for index := range left {
		leftMean += left[index]
		rightMean += right[index]
	}
	leftMean /= float64(len(left))
	rightMean /= float64(len(right))
	numerator, leftPower, rightPower := 0.0, 0.0, 0.0
	for index := range left {
		leftValue := left[index] - leftMean
		rightValue := right[index] - rightMean
		numerator += leftValue * rightValue
		leftPower += leftValue * leftValue
		rightPower += rightValue * rightValue
	}
	if leftPower == 0 || rightPower == 0 {
		return 0
	}
	return numerator / math.Sqrt(leftPower*rightPower)
}

func samplesDuration(samples int) time.Duration {
	return time.Duration(samples) * time.Second / 8000
}

func durationSamples(duration time.Duration) int {
	return int(duration * 8000 / time.Second)
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
