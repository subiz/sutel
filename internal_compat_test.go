package sutel

// Config and Carrier keep the existing black-box transaction tests focused on
// protocol behavior while the public API creates one unexported carrier per
// Session. They only exist in this package's test build.
type Config = runtimeConfig
type Carrier = carrier

func NewCarrier(config Config) (*Carrier, error) {
	defaults, err := newRuntimeConfig(config.LocalIP, config.LocalPort)
	if err != nil {
		return nil, err
	}
	if config.SIPT1 != 0 {
		defaults.SIPT1 = config.SIPT1
	}
	if config.SIPT2 != 0 {
		defaults.SIPT2 = config.SIPT2
	}
	if config.RTPDrainTimeout != 0 {
		defaults.RTPDrainTimeout = config.RTPDrainTimeout
	}
	if config.RTPReorderWindow != 0 {
		defaults.RTPReorderWindow = config.RTPReorderWindow
	}
	if config.MaxSynthesizedRTPGap != 0 {
		defaults.MaxSynthesizedRTPGap = config.MaxSynthesizedRTPGap
	}
	if config.MaxSIPMessageBytes != 0 {
		defaults.MaxSIPMessageBytes = config.MaxSIPMessageBytes
	}
	return newCarrier(defaults)
}
