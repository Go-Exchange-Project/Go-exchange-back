package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/Go-Exchange-Project/Go-exchange-back/internal/repository"
)

const (
	defaultTransferPollInterval = 5 * time.Second
	defaultTransferPollBatch    = 64
)

// TransferStatusPoller는 두 가지를 한다: RECEIVED 요청의 제출 재시도, PROCESSING
// 요청의 상태 조회. 시간 경과만으로 돈을 반환하는 규칙은 없다 — 시간 임계는
// review_required_at을 켜는 데만 쓰인다(§8.6).
type TransferStatusPoller struct {
	Transfers *repository.TransferRepository
	Service   *TransferService
	Interval  time.Duration
	Batch     int
	Logger    *log.Logger
}

func (p *TransferStatusPoller) Run(ctx context.Context) {
	p.RunOnce()
	ticker := time.NewTicker(p.interval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.RunOnce()
		}
	}
}

func (p *TransferStatusPoller) RunOnce() {
	requests, err := p.Transfers.DueForCheck(p.now(), p.batch())
	if err != nil {
		p.logf("transfer poller: due-for-check scan failed: %v", err)
		return
	}

	for _, request := range requests {
		switch request.Status {
		case model.TransferStatusReceived:
			if dispatchErr := p.Service.Dispatch(request); dispatchErr != nil {
				p.logf("transfer poller: dispatch failed for request %d: %v", request.ID, dispatchErr)
			}
		case model.TransferStatusProcessing:
			p.checkStatus(request)
		}
	}
}

func (p *TransferStatusPoller) checkStatus(request model.TransferRequest) {
	if request.ExternalRef == nil {
		p.logf("transfer poller: processing request %d has no external_ref", request.ID)
		return
	}

	outcome, payload, err := p.Service.Processor.GetTransferStatus(*request.ExternalRef)
	if err != nil {
		p.logf("transfer poller: status check failed for request %d: %v", request.ID, err)
		// §8.6 백오프대로 조회 일정을 전진시킨다 — 건드리지 않으면 외부가
		// 불안정한 바로 그때 매 틱마다 재조회하는 조회 폭주가 된다. 임계를
		// 넘기면 확인 표시도 켠다 — 그러지 않으면 출금 자금이 아무도 모르게
		// 잠긴 채 남는다.
		if scheduleErr := p.Service.HandleStatusCheckFailure(request.ID); scheduleErr != nil {
			p.logf("transfer poller: advance poll schedule after status check failure failed for request %d: %v", request.ID, scheduleErr)
		}
		return
	}

	eventKey := fmt.Sprintf("poll:%d:%d", request.ID, request.CheckAttempts+1)
	if outcome.IsTerminal() {
		if resolveErr := p.Service.ResolveTransfer(ResolveInput{
			TransferRequestID: request.ID,
			Source:            model.TransferEventSourcePoll,
			EventKey:          eventKey,
			Outcome:           outcome,
			Payload:           payload,
		}); resolveErr != nil {
			p.logf("transfer poller: resolve failed for request %d: %v", request.ID, resolveErr)
		}
		return
	}

	if obsErr := p.Service.RecordObservation(ObservationInput{
		TransferRequestID: request.ID,
		EventKey:          eventKey,
		Outcome:           outcome,
		Payload:           payload,
	}); obsErr != nil {
		p.logf("transfer poller: record observation failed for request %d: %v", request.ID, obsErr)
	}
}

func (p *TransferStatusPoller) now() time.Time {
	if p.Service != nil && p.Service.Now != nil {
		return p.Service.Now()
	}
	return time.Now()
}

func (p *TransferStatusPoller) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return defaultTransferPollInterval
}

func (p *TransferStatusPoller) batch() int {
	if p.Batch > 0 {
		return p.Batch
	}
	return defaultTransferPollBatch
}

func (p *TransferStatusPoller) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
