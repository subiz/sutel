package sip

import "testing"

func FuzzParseNeverPanics(f *testing.F) {
	f.Add([]byte("OPTIONS sip:127.0.0.1 SIP/2.0\r\nContent-Length: 0\r\n\r\n"))
	f.Add([]byte("SIP/2.0 200 \nContent-Length: 0\n\n"))
	f.Add([]byte{0, 1, 2, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data, 64*1024)
	})
}
