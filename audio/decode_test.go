package audio

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDecodeProvidedSampleWAV(t *testing.T) {
	path := filepath.Join("..", "testdata", "sample.wav")
	pcm, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if pcm.SampleRate != 44100 {
		t.Fatalf("sample rate=%d want 44100", pcm.SampleRate)
	}
	if len(pcm.Samples) < 44100 {
		t.Fatalf("sample contains only %d frames", len(pcm.Samples))
	}
	normalized, err := Normalize8kMono(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) < 8000 {
		t.Fatalf("normalized sample contains only %d samples", len(normalized))
	}
}

func TestWAVWriteReadRoundTrip(t *testing.T) {
	want := []int16{-32768, -1000, 0, 1000, 32767}
	var encoded bytes.Buffer
	if err := WriteWAV(&encoded, want, 8000); err != nil {
		t.Fatal(err)
	}
	got, err := DecodeWAV(encoded.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.SampleRate != 8000 || len(got.Samples) != len(want) {
		t.Fatalf("unexpected WAV metadata: %+v", got)
	}
	for index := range want {
		if got.Samples[index] != want[index] {
			t.Fatalf("sample[%d]=%d want %d", index, got.Samples[index], want[index])
		}
	}
}

func TestLoadRejectsMP3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio.mp3")
	if err := os.WriteFile(path, []byte("not mp3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted MP3")
	}
}

func TestResample(t *testing.T) {
	input := make([]int16, 44100)
	for index := range input {
		input[index] = int16(index % 1000)
	}
	output := Resample(input, 44100, 8000)
	if len(output) != 8000 {
		t.Fatalf("resampled length=%d want 8000", len(output))
	}
}

func TestDecodeWAVCommonPCMDepthsAndStereoDownmix(t *testing.T) {
	tests := []struct {
		name     string
		bits     uint16
		channels uint16
		data     []byte
		want     []int16
	}{
		{name: "8-bit mono", bits: 8, channels: 1, data: []byte{0, 128, 255}, want: []int16{-32768, 0, 32512}},
		{name: "16-bit stereo", bits: 16, channels: 2, data: pcmBytes16(-1000, 1000, 2000, 4000), want: []int16{0, 3000}},
		{name: "24-bit mono", bits: 24, channels: 1, data: []byte{0, 0, 0x80, 0, 0, 0, 0xff, 0xff, 0x7f}, want: []int16{-32768, 0, 32767}},
		{name: "32-bit mono", bits: 32, channels: 1, data: pcmBytes32(-2147483648, 0, 2147483647), want: []int16{-32768, 0, 32767}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoded, err := DecodeWAV(wavFixture(test.bits, test.channels, test.data, true))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(decoded.Samples, test.want) {
				t.Fatalf("samples=%v want %v", decoded.Samples, test.want)
			}
		})
	}
}

func TestDecodeWAVRejectsMalformedChunks(t *testing.T) {
	valid := wavFixture(16, 1, pcmBytes16(1, 2), true)
	tests := map[string][]byte{
		"missing fmt":     wavFixture(16, 1, pcmBytes16(1), false),
		"truncated data":  append([]byte(nil), valid[:len(valid)-1]...),
		"lying data size": append([]byte(nil), valid...),
		"lying RIFF size": append([]byte(nil), valid...),
	}
	binary.LittleEndian.PutUint32(tests["lying data size"][40:44], 1000)
	binary.LittleEndian.PutUint32(tests["lying RIFF size"][4:8], 1)
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeWAV(data); err == nil {
				t.Fatal("accepted malformed WAV")
			}
		})
	}
}

func wavFixture(bits, channels uint16, data []byte, includeFormat bool) []byte {
	var chunks bytes.Buffer
	if includeFormat {
		chunks.WriteString("fmt ")
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(16))
		_ = binary.Write(&chunks, binary.LittleEndian, uint16(1))
		_ = binary.Write(&chunks, binary.LittleEndian, channels)
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(8000))
		bytesPerFrame := uint32(channels) * uint32(bits/8)
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(8000)*bytesPerFrame)
		_ = binary.Write(&chunks, binary.LittleEndian, uint16(bytesPerFrame))
		_ = binary.Write(&chunks, binary.LittleEndian, bits)
	}
	chunks.WriteString("data")
	_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(data)))
	chunks.Write(data)
	if len(data)%2 != 0 {
		chunks.WriteByte(0)
	}
	var result bytes.Buffer
	result.WriteString("RIFF")
	_ = binary.Write(&result, binary.LittleEndian, uint32(4+chunks.Len()))
	result.WriteString("WAVE")
	result.Write(chunks.Bytes())
	return result.Bytes()
}

func pcmBytes16(values ...int16) []byte {
	data := make([]byte, len(values)*2)
	for index, value := range values {
		binary.LittleEndian.PutUint16(data[index*2:], uint16(value))
	}
	return data
}

func pcmBytes32(values ...int32) []byte {
	data := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(data[index*4:], uint32(value))
	}
	return data
}
