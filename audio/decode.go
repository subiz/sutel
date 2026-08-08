package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type PCM struct {
	Samples    []int16
	SampleRate int
}

func Load(path string) (PCM, error) {
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".wav":
		return DecodeWAVFile(path)
	default:
		return PCM{}, fmt.Errorf("unsupported audio extension %q; convert input to uncompressed PCM WAV", extension)
	}
}

func Normalize8kMono(path string) ([]int16, error) {
	decoded, err := Load(path)
	if err != nil {
		return nil, err
	}
	return Resample(decoded.Samples, decoded.SampleRate, 8000), nil
}

func DecodeWAVFile(path string) (PCM, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PCM{}, fmt.Errorf("read WAV: %w", err)
	}
	return DecodeWAV(data)
}

func DecodeWAV(data []byte) (PCM, error) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return PCM{}, fmt.Errorf("invalid WAV header")
	}
	declaredSize := int64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if declaredSize != int64(len(data)) {
		return PCM{}, fmt.Errorf("invalid WAV RIFF size")
	}
	var format, channels, bits uint16
	var sampleRate uint32
	var audio []byte
	for offset := 12; offset+8 <= len(data); {
		name := string(data[offset : offset+4])
		size := int64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end64 := int64(start) + size
		if end64 > int64(len(data)) {
			return PCM{}, fmt.Errorf("invalid WAV chunk %q", name)
		}
		end := int(end64)
		switch name {
		case "fmt ":
			if size < 16 {
				return PCM{}, fmt.Errorf("invalid WAV fmt chunk")
			}
			format = binary.LittleEndian.Uint16(data[start:])
			channels = binary.LittleEndian.Uint16(data[start+2:])
			sampleRate = binary.LittleEndian.Uint32(data[start+4:])
			bits = binary.LittleEndian.Uint16(data[start+14:])
		case "data":
			audio = data[start:end]
		}
		padding := int(size % 2)
		if end+padding > len(data) {
			return PCM{}, fmt.Errorf("invalid WAV chunk padding")
		}
		offset = end + padding
	}
	if format != 1 || channels == 0 || sampleRate == 0 || len(audio) == 0 {
		return PCM{}, fmt.Errorf("unsupported WAV format=%d channels=%d rate=%d", format, channels, sampleRate)
	}
	bytesPerSample := int(bits / 8)
	if bits != 8 && bits != 16 && bits != 24 && bits != 32 {
		return PCM{}, fmt.Errorf("unsupported WAV PCM depth %d", bits)
	}
	frameSize := bytesPerSample * int(channels)
	if len(audio)%frameSize != 0 {
		return PCM{}, fmt.Errorf("invalid WAV data length")
	}
	mono := make([]int16, len(audio)/frameSize)
	for frame := range mono {
		var total int64
		for channel := 0; channel < int(channels); channel++ {
			offset := frame*frameSize + channel*bytesPerSample
			total += int64(readPCMSample(audio[offset:offset+bytesPerSample], bits))
		}
		mono[frame] = int16(total / int64(channels))
	}
	return PCM{Samples: mono, SampleRate: int(sampleRate)}, nil
}

func readPCMSample(data []byte, bits uint16) int32 {
	switch bits {
	case 8:
		return (int32(data[0]) - 128) << 8
	case 16:
		return int32(int16(binary.LittleEndian.Uint16(data)))
	case 24:
		value := int32(data[0]) | int32(data[1])<<8 | int32(data[2])<<16
		if value&0x800000 != 0 {
			value |= ^0xffffff
		}
		return value >> 8
	default:
		return int32(binary.LittleEndian.Uint32(data)) >> 16
	}
}

// Resample uses deterministic linear interpolation. It intentionally does not
// apply a low-pass filter, so content above the destination Nyquist frequency
// may alias when downsampling.
func Resample(samples []int16, fromRate, toRate int) []int16 {
	if len(samples) == 0 || fromRate <= 0 || toRate <= 0 {
		return nil
	}
	if fromRate == toRate {
		return append([]int16(nil), samples...)
	}
	length := int(math.Round(float64(len(samples)) * float64(toRate) / float64(fromRate)))
	if length < 1 {
		length = 1
	}
	result := make([]int16, length)
	for index := range result {
		position := float64(index) * float64(fromRate) / float64(toRate)
		left := int(position)
		if left >= len(samples)-1 {
			result[index] = samples[len(samples)-1]
			continue
		}
		fraction := position - float64(left)
		value := float64(samples[left])*(1-fraction) + float64(samples[left+1])*fraction
		result[index] = int16(math.Round(value))
	}
	return result
}

func WriteWAV(writer io.Writer, samples []int16, sampleRate int) error {
	dataSize := uint32(len(samples) * 2)
	header := new(bytes.Buffer)
	header.WriteString("RIFF")
	binary.Write(header, binary.LittleEndian, uint32(36)+dataSize)
	header.WriteString("WAVEfmt ")
	binary.Write(header, binary.LittleEndian, uint32(16))
	binary.Write(header, binary.LittleEndian, uint16(1))
	binary.Write(header, binary.LittleEndian, uint16(1))
	binary.Write(header, binary.LittleEndian, uint32(sampleRate))
	binary.Write(header, binary.LittleEndian, uint32(sampleRate*2))
	binary.Write(header, binary.LittleEndian, uint16(2))
	binary.Write(header, binary.LittleEndian, uint16(16))
	header.WriteString("data")
	binary.Write(header, binary.LittleEndian, dataSize)
	if _, err := writer.Write(header.Bytes()); err != nil {
		return err
	}
	return binary.Write(writer, binary.LittleEndian, samples)
}
