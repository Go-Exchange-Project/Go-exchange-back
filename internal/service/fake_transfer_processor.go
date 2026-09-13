package service

import (
	"fmt"
	"sync"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
)

// FakeTransferProcessor는 가짜 은행·가짜 체인을 흉내 낸다. 실제 시간이나 네트워크는
// 쓰지 않는다 — 결과는 SetResult로 미리 심어 두거나 기본값(PENDING)으로 남는다.
//
// Submit은 dispatchKey로 결정적이다: ref = "FAKE-{rail}-{dispatchKey}"다.
// dispatchKey는 "transfer:{id}"이고 id는 DB 시퀀스라 전역 유일하므로, ref 생성에
// 메모리 상태(카운터 등)가 전혀 필요 없다 — 이것이 Submit의 멱등 계약이 실제로
// 의미 있는 크래시 상황(프로세스가 죽었다 재기동해 새 FakeTransferProcessor
// 인스턴스로 같은 dispatchKey를 다시 제출하는 경우)에서도 성립하게 한다.
type FakeTransferProcessor struct {
	mu           sync.Mutex
	outcomeByRef map[string]fakeTransferOutcome
}

type fakeTransferOutcome struct {
	outcome model.TransferOutcome
	payload map[string]any
}

func NewFakeTransferProcessor() *FakeTransferProcessor {
	return &FakeTransferProcessor{
		outcomeByRef: map[string]fakeTransferOutcome{},
	}
}

func (p *FakeTransferProcessor) Submit(dispatchKey string, req model.TransferRequest) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ref := fmt.Sprintf("FAKE-%s-%s", req.Rail, dispatchKey)
	if _, ok := p.outcomeByRef[ref]; !ok {
		p.outcomeByRef[ref] = fakeTransferOutcome{outcome: model.TransferOutcomePending}
	}
	return ref, nil
}

// GetTransferStatus는 모르는 ref에도 오류를 내지 않고 PENDING을 돌려준다 — 이
// 처리기의 문서화된 기본값이 PENDING이고, 재기동으로 메모리가 비어도 가짜 외부
// 시스템은 여전히 "아직 처리 중"으로 보이는 것이 맞다. 실제 외부 처리기는 진짜로
// 응답하지 않을 수 있으므로, 그 오류 경로는 TransferStatusPoller가 따로 다룬다.
func (p *FakeTransferProcessor) GetTransferStatus(externalRef string) (model.TransferOutcome, map[string]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	result, ok := p.outcomeByRef[externalRef]
	if !ok {
		return model.TransferOutcomePending, nil, nil
	}
	return result.outcome, result.payload, nil
}

// SetResult는 테스트 전용이다. 이후 GetTransferStatus 호출이 이 값을 돌려준다 —
// 시계를 흉내 내지 않고, 테스트가 콜백처럼 결과를 직접 심어 둔다.
func (p *FakeTransferProcessor) SetResult(externalRef string, outcome model.TransferOutcome, payload map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outcomeByRef[externalRef] = fakeTransferOutcome{outcome: outcome, payload: payload}
}
