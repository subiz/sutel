package sutel

import (
	"fmt"
	"strings"
	"time"

	"github.com/subiz/sutel/internal/sip"
)

type dialogTermination uint8

const (
	dialogStillActive dialogTermination = iota
	dialogTerminatedByPeer
	dialogNeedsLocalBYE
)

func (s *Session) runEstablishedDialog(dialog *callDialog, hangupAfter time.Duration, localHangup <-chan struct{}) (dialogTermination, error) {
	var hangupTimer transactionTimer
	var deadline <-chan time.Time
	if hangupAfter > 0 {
		hangupTimer = s.carrier.clock.NewTimer(hangupAfter)
		deadline = timerChannel(hangupTimer)
		defer stopTransactionTimer(hangupTimer)
	}
	for {
		message, source, timeout, err := s.readSIPWithTimers(s.ctx, nil, deadline, localHangup)
		if err != nil {
			return dialogStillActive, err
		}
		if timeout == deadlineSIPReadTimeout || timeout == localHangupSIPReadTimeout {
			return dialogNeedsLocalBYE, nil
		}
		terminated, err := s.handleDialogRequest(message, source, dialog)
		if err != nil {
			return dialogStillActive, err
		}
		if terminated {
			return dialogTerminatedByPeer, nil
		}
	}
}

func (s *Session) runEstablishedDialogUntilNetworkLoss(dialog *callDialog, after time.Duration) error {
	timer := s.carrier.clock.NewTimer(after)
	defer stopTransactionTimer(timer)
	for {
		message, source, timeout, err := s.readSIPWithTimers(s.ctx, nil, timerChannel(timer), nil)
		if err != nil {
			return err
		}
		if timeout == deadlineSIPReadTimeout {
			return nil
		}
		terminated, err := s.handleDialogRequest(message, source, dialog)
		if err != nil || terminated {
			return err
		}
	}
}

func (s *Session) sendDialogBYE(dialog *callDialog) (bool, error) {
	dialog.localCSeq++
	request := sip.NewRequest("BYE", dialog.remoteTarget)
	appendRequestHeaders(request, s.carrier.ids.Branch(), dialog.localAddress, dialog.remoteAddress, dialog.callID, dialog.localCSeq, localContact(s.sipAddr, "sutel"))
	return s.sendNonInviteTransaction(request, dialog, nil)
}

func (s *Session) sendDialogINFO(dialog *callDialog, digit string) (bool, error) {
	dialog.localCSeq++
	request := sip.NewRequest("INFO", dialog.remoteTarget)
	appendRequestHeaders(request, s.carrier.ids.Branch(), dialog.localAddress, dialog.remoteAddress, dialog.callID, dialog.localCSeq, localContact(s.sipAddr, "sutel"))
	request.Add("Content-Type", "application/dtmf-relay")
	request.Body = []byte(fmt.Sprintf("Signal=%s\r\nDuration=160\r\n", strings.ToUpper(digit)))
	return s.sendNonInviteTransaction(request, dialog, nil)
}

func (s *Session) sendNonInviteTransaction(request *sip.Message, dialog *callDialog, onRequest func(*sip.Message) error) (bool, error) {
	target, err := routeDialogRequest(request, dialog)
	if err != nil {
		return false, err
	}
	if err := s.sendSIP(request, target, false); err != nil {
		return false, err
	}
	cseq, _ := request.CSeq()
	interval := s.carrier.config.SIPT1
	retransmitTimer := s.carrier.clock.NewTimer(interval)
	defer func() { stopTransactionTimer(retransmitTimer) }()
	for {
		message, source, timeout, err := s.readSIPWithTimers(s.ctx, timerChannel(retransmitTimer), nil, nil)
		if err != nil {
			return false, err
		}
		if timeout == retransmitSIPReadTimeout {
			if err := s.sendSIP(request, target, true); err != nil {
				return false, err
			}
			stopTransactionTimer(retransmitTimer)
			interval = min(interval*2, s.carrier.config.SIPT2)
			retransmitTimer = s.carrier.clock.NewTimer(interval)
			continue
		}
		if !message.IsRequest() && isMatching(message, dialog.callID, cseq.Number, request.Method) {
			if message.StatusCode >= 200 && message.StatusCode < 300 {
				return false, nil
			}
			if message.StatusCode >= 300 {
				return false, fmt.Errorf("%w: %s received %d", ErrProtocol, request.Method, message.StatusCode)
			}
			continue
		}
		if message.IsRequest() {
			if onRequest != nil {
				if err := onRequest(message); err != nil {
					return false, err
				}
			}
			terminated, err := s.handleDialogRequest(message, source, dialog)
			if err != nil {
				return false, err
			}
			if terminated {
				return true, nil
			}
			continue
		}
		s.ignoreMessage()
	}
}

// sendDialogBYEOnce is best-effort cleanup for a dialog established by a 2xx
// response whose verification or SDP processing subsequently failed.
func (s *Session) sendDialogBYEOnce(dialog *callDialog) error {
	dialog.localCSeq++
	request := sip.NewRequest("BYE", dialog.remoteTarget)
	appendRequestHeaders(request, s.carrier.ids.Branch(), dialog.localAddress, dialog.remoteAddress, dialog.callID, dialog.localCSeq, localContact(s.sipAddr, "sutel"))
	target, err := routeDialogRequest(request, dialog)
	if err != nil {
		return err
	}
	return s.sendSIP(request, target, false)
}

func (s *Session) waitDialogDuration(dialog *callDialog, duration time.Duration) (bool, error) {
	if duration <= 0 {
		return false, nil
	}
	timer := s.carrier.clock.NewTimer(duration)
	defer stopTransactionTimer(timer)
	for {
		message, source, timeout, err := s.readSIPWithTimers(s.ctx, nil, timerChannel(timer), nil)
		if err != nil {
			return false, err
		}
		if timeout == deadlineSIPReadTimeout {
			return false, nil
		}
		terminated, err := s.handleDialogRequest(message, source, dialog)
		if err != nil || terminated {
			return terminated, err
		}
	}
}

func (s *Session) finishLocalDialog(dialog *callDialog) error {
	peerTerminated, err := s.sendDialogBYE(dialog)
	if err == nil && !peerTerminated {
		s.setOutcome(func(outcome *SIPOutcome) { outcome.TerminatedBy = Sutel })
	}
	return err
}
