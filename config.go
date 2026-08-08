package sutel

import (
	"fmt"
	"net/netip"
	"time"
)

type runtimeConfig struct {
	LocalIP   string
	LocalPort uint16

	SIPT1                time.Duration
	SIPT2                time.Duration
	RTPDrainTimeout      time.Duration
	RTPReorderWindow     int
	MaxSynthesizedRTPGap time.Duration
	MaxSIPMessageBytes   int
}

const (
	defaultTimeout           = 20 * time.Second
	defaultSIPT1             = 500 * time.Millisecond
	defaultSIPT2             = 4 * time.Second
	defaultRTPDrainTimeout   = 200 * time.Millisecond
	defaultRTPReorderWindow  = 10
	defaultMaxRTPGap         = 2 * time.Second
	defaultMaxSIPMessageSize = 64 * 1024
)

func newRuntimeConfig(localIP string, localPort uint16) (runtimeConfig, error) {
	c := runtimeConfig{LocalIP: localIP, LocalPort: localPort}
	if c.LocalIP == "" {
		c.LocalIP = "127.0.0.1"
	}
	address, err := netip.ParseAddr(c.LocalIP)
	if err != nil || !address.IsValid() || !address.Is4() {
		return runtimeConfig{}, fmt.Errorf("LocalIP: invalid address %q", c.LocalIP)
	}
	if address.IsUnspecified() || address.IsMulticast() {
		return runtimeConfig{}, fmt.Errorf("LocalIP: unsafe address %q is not allowed", c.LocalIP)
	}
	c.SIPT1 = defaultSIPT1
	c.SIPT2 = defaultSIPT2
	c.RTPDrainTimeout = defaultRTPDrainTimeout
	c.RTPReorderWindow = defaultRTPReorderWindow
	c.MaxSynthesizedRTPGap = defaultMaxRTPGap
	c.MaxSIPMessageBytes = defaultMaxSIPMessageSize
	return c, nil
}

func (c runtimeConfig) validate() error {
	if c.SIPT1 <= 0 || c.SIPT2 < c.SIPT1 || c.RTPDrainTimeout <= 0 || c.MaxSynthesizedRTPGap <= 0 {
		return fmt.Errorf("invalid internal durations")
	}
	if c.RTPReorderWindow <= 0 || c.MaxSIPMessageBytes <= 0 {
		return fmt.Errorf("invalid internal sizes")
	}
	return nil
}
