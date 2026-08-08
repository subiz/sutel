package audio

import (
	"math"
	"testing"
)

func TestALawFullByteDomain(t *testing.T) {
	for value := 0; value <= 255; value++ {
		decoded := DecodeALaw(byte(value))
		roundTrip := DecodeALaw(EncodeALaw(decoded))
		if roundTrip != decoded {
			t.Fatalf("A-law byte %#02x decoded=%d round-trip=%d", value, decoded, roundTrip)
		}
	}
}

func TestMuLawFullByteDomain(t *testing.T) {
	for value := 0; value <= 255; value++ {
		decoded := DecodeMuLaw(byte(value))
		roundTrip := DecodeMuLaw(EncodeMuLaw(decoded))
		if roundTrip != decoded {
			t.Fatalf("mu-law byte %#02x decoded=%d round-trip=%d", value, decoded, roundTrip)
		}
	}
}

func TestG711KnownZeroCodes(t *testing.T) {
	if got := EncodeALaw(0); got != 0xd5 {
		t.Fatalf("EncodeALaw(0)=%#x want %#x", got, 0xd5)
	}
	if got := EncodeMuLaw(0); got != 0xff {
		t.Fatalf("EncodeMuLaw(0)=%#x want %#x", got, 0xff)
	}
}

func TestG711GoldenDecodePoints(t *testing.T) {
	aLaw := map[byte]int16{
		0xd5: 8, 0x55: -8, 0xaa: 32256, 0x2a: -32256, 0x80: 5504, 0x00: -5504,
	}
	for encoded, want := range aLaw {
		if got := DecodeALaw(encoded); got != want {
			t.Fatalf("DecodeALaw(%#02x)=%d want %d", encoded, got, want)
		}
	}
	muLaw := map[byte]int16{
		0xff: 0, 0x7f: 0, 0x80: 32124, 0x00: -32124,
	}
	for encoded, want := range muLaw {
		if got := DecodeMuLaw(encoded); got != want {
			t.Fatalf("DecodeMuLaw(%#02x)=%d want %d", encoded, got, want)
		}
	}
}

func TestG711EncodeWholePCM16DomainIsStable(t *testing.T) {
	for value := -32768; value <= 32767; value++ {
		sample := int16(value)
		for _, codec := range []string{"PCMA", "PCMU"} {
			encoded := EncodeG711([]int16{sample}, codec)[0]
			decoded := DecodeG711([]byte{encoded}, codec)[0]
			if codec == "PCMU" && decoded == 0 && (encoded == 0x7f || encoded == 0xff) {
				continue // G.711 μ-law has equivalent positive and negative zero codes.
			}
			if encodedAgain := EncodeG711([]int16{decoded}, codec)[0]; encodedAgain != encoded {
				t.Fatalf("%s sample=%d code=%#02x decoded=%d re-encoded=%#02x", codec, sample, encoded, decoded, encodedAgain)
			}
		}
	}
}

func TestG711SignalQuality(t *testing.T) {
	samples := make([]int16, 8000)
	for index := range samples {
		samples[index] = int16(20000 * math.Sin(2*math.Pi*440*float64(index)/8000))
	}
	for _, codec := range []string{"PCMA", "PCMU"} {
		decoded := DecodeG711(EncodeG711(samples, codec), codec)
		var signal, noise float64
		for index := range samples {
			signal += float64(samples[index]) * float64(samples[index])
			delta := float64(samples[index]) - float64(decoded[index])
			noise += delta * delta
		}
		snr := 10 * math.Log10(signal/noise)
		if snr < 30 {
			t.Fatalf("%s SNR %.2f dB is too low", codec, snr)
		}
	}
}
