package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/subiz/sutel"
)

func main() {
	file := "testdata/sample.wav"
	if len(os.Args) > 1 {
		file = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	call, err := sutel.ExpectOutboundCall(ctx, sutel.OutboundScenario{
		LocalIP:   "127.0.0.1",
		LocalPort: 5060,
		Timeout:   5 * time.Minute,
		To:        "200",
		Behavior: sutel.Answer{
			Playback: &sutel.AudioPlayback{File: file},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer call.Close()

	log.Printf("Sutel listening at sip:200@%s; waiting for pjsua", call.SIPAddr())
	result, err := call.Wait()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Fatal(err)
	}
	log.Printf("call finished: final=%d RTP-sent=%d", result.Outcome.InviteFinalStatus, result.RTP.PacketsSent)
}
