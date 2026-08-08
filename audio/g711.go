package audio

const (
	muLawBias = 0x84
	muLawClip = 32635
)

func DecodeALaw(value byte) int16 {
	value ^= 0x55
	magnitude := int(value&0x0f) << 4
	segment := int(value&0x70) >> 4
	switch segment {
	case 0:
		magnitude += 8
	case 1:
		magnitude += 0x108
	default:
		magnitude += 0x108
		magnitude <<= segment - 1
	}
	if value&0x80 != 0 {
		return int16(magnitude)
	}
	return int16(-magnitude)
}

func EncodeALaw(sample int16) byte {
	pcm := int(sample) >> 3
	mask := byte(0xd5)
	if pcm < 0 {
		mask = 0x55
		pcm = -pcm - 1
	}
	if pcm > 4095 {
		pcm = 4095
	}
	segment := searchSegment(pcm, [...]int{0x1f, 0x3f, 0x7f, 0xff, 0x1ff, 0x3ff, 0x7ff, 0xfff})
	if segment >= 8 {
		return 0x7f ^ mask
	}
	encoded := byte(segment << 4)
	if segment < 2 {
		encoded |= byte((pcm >> 1) & 0x0f)
	} else {
		encoded |= byte((pcm >> segment) & 0x0f)
	}
	return encoded ^ mask
}

func DecodeMuLaw(value byte) int16 {
	value = ^value
	magnitude := ((int(value&0x0f) << 3) + muLawBias) << ((value & 0x70) >> 4)
	magnitude -= muLawBias
	if value&0x80 != 0 {
		return int16(-magnitude)
	}
	return int16(magnitude)
}

func EncodeMuLaw(sample int16) byte {
	pcm := int(sample)
	mask := byte(0xff)
	if pcm < 0 {
		mask = 0x7f
		pcm = -pcm
	}
	if pcm > muLawClip {
		pcm = muLawClip
	}
	pcm += muLawBias
	segment := searchSegment(pcm, [...]int{0xff, 0x1ff, 0x3ff, 0x7ff, 0xfff, 0x1fff, 0x3fff, 0x7fff})
	encoded := byte(segment<<4) | byte((pcm>>(segment+3))&0x0f)
	return encoded ^ mask
}

func searchSegment(value int, ends [8]int) int {
	for index, end := range ends {
		if value <= end {
			return index
		}
	}
	return 8
}

func DecodeG711(payload []byte, codec string) []int16 {
	result := make([]int16, len(payload))
	for index, value := range payload {
		if codec == "PCMU" {
			result[index] = DecodeMuLaw(value)
		} else {
			result[index] = DecodeALaw(value)
		}
	}
	return result
}

func EncodeG711(samples []int16, codec string) []byte {
	result := make([]byte, len(samples))
	for index, sample := range samples {
		if codec == "PCMU" {
			result[index] = EncodeMuLaw(sample)
		} else {
			result[index] = EncodeALaw(sample)
		}
	}
	return result
}
