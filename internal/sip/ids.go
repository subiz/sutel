package sip

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"io"
	"sync"
	"sync/atomic"
)

type IDGenerator struct {
	mu     sync.Mutex
	reader io.Reader
}

var fallbackID atomic.Uint64

func NewIDGenerator(reader io.Reader) *IDGenerator {
	if reader == nil {
		reader = rand.Reader
	}
	return &IDGenerator{reader: reader}
}

func (generator *IDGenerator) ID(prefix string) string {
	var value [12]byte
	generator.mu.Lock()
	_, err := io.ReadFull(generator.reader, value[:])
	generator.mu.Unlock()
	if err != nil {
		// Identifier generation must never crash a test process. The atomic
		// fallback remains collision-free within this process if the operating
		// system random source becomes unavailable.
		binary.BigEndian.PutUint64(value[4:], fallbackID.Add(1))
	}
	return prefix + hex.EncodeToString(value[:])
}

func (generator *IDGenerator) Tag() string    { return generator.ID("t-") }
func (generator *IDGenerator) Branch() string { return "z9hG4bK-" + generator.ID("") }
func (generator *IDGenerator) CallID() string { return generator.ID("c-") + "@sutel" }

var defaultIDs = NewIDGenerator(nil)

func ID(prefix string) string { return defaultIDs.ID(prefix) }
func Tag() string             { return defaultIDs.Tag() }
func Branch() string          { return defaultIDs.Branch() }
func CallID() string          { return defaultIDs.CallID() }
