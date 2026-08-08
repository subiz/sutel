package sutel

// CallDirection describes the direction from the system under test's point of
// view.
type CallDirection uint8

const (
	Outbound CallDirection = iota
	Inbound
)

// CallParty identifies which endpoint terminated an established call.
type CallParty uint8

const (
	NoParty CallParty = iota
	SystemUnderTest
	Sutel
)

func (p CallParty) String() string {
	switch p {
	case SystemUnderTest:
		return "system-under-test"
	case Sutel:
		return "sutel"
	default:
		return "none"
	}
}

// Codec is an audio codec supported by Sutel.
type Codec uint8

const (
	PCMA Codec = iota
	PCMU
)

func (c Codec) String() string {
	switch c {
	case PCMA:
		return "PCMA"
	case PCMU:
		return "PCMU"
	default:
		return "unknown"
	}
}

func (c Codec) PayloadType() uint8 {
	if c == PCMU {
		return 0
	}
	return 8
}
