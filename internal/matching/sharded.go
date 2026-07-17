package matching

import (
	"sync"
	"sync/atomic"
	"time"
)

// ShardedEngine은 심볼을 N개의 독립 MatchingEngine에 분배하는 라우터다.
// MatchingEngine 자체는 무수정 — 한 심볼은 항상 같은 샤드가 소유하므로 심볼 내
// 순서·무락 오더북 접근이라는 기존 불변식이 샤드 단위로 그대로 성립한다.
type ShardedEngine struct {
	shards      []*MatchingEngine
	assignments sync.Map // coinSymbol -> *MatchingEngine
	nextShard   atomic.Uint64

	// ExecutionCh/SnapshotCh는 전 샤드의 이벤트를 모으는 팬인(머지) 채널이다.
	// 각 샤드는 여전히 자기 채널을 갖고 자기 채널을 닫으며(엔진 무수정), 이
	// 채널들의 close는 Start()가 띄우는 포워더 goroutine들이 전부 끝난 뒤
	// ShardedEngine이 1회만 수행한다.
	ExecutionCh chan ExecutionEvent
	SnapshotCh  chan OrderBookSnapshot

	stopOnce sync.Once
	doneCh   chan struct{}
}

// NewShardedEngine은 shardCount개의 독립 MatchingEngine을 생성해 소유한다.
// 빈 심볼("")은 라운드로빈 카운터를 소비하지 않고 항상 샤드 0에 고정된다.
func NewShardedEngine(shardCount int) *ShardedEngine {
	if shardCount < 1 {
		shardCount = 1
	}
	shards := make([]*MatchingEngine, shardCount)
	for i := range shards {
		shards[i] = NewMatchingEngine()
	}
	se := &ShardedEngine{
		shards:      shards,
		ExecutionCh: make(chan ExecutionEvent, 1024),
		SnapshotCh:  make(chan OrderBookSnapshot, 256),
		doneCh:      make(chan struct{}),
	}
	se.assignments.Store("", shards[0])
	return se
}

// shardFor는 심볼의 소유 샤드를 반환한다. 처음 보는 심볼은 라운드로빈으로 다음
// 후보 샤드를 골라 sync.Map에 고정한다(LoadOrStore) — 동시에 같은 신규 심볼이
// 여러 goroutine에서 들어와도 먼저 저장된 쪽만 이겨 배정이 유일하게 안정된다.
func (se *ShardedEngine) shardFor(coinSymbol string) *MatchingEngine {
	if value, ok := se.assignments.Load(coinSymbol); ok {
		return value.(*MatchingEngine)
	}
	candidateIdx := int(se.nextShard.Add(1)-1) % len(se.shards)
	candidate := se.shards[candidateIdx]
	actual, _ := se.assignments.LoadOrStore(coinSymbol, candidate)
	return actual.(*MatchingEngine)
}

// Start는 전 샤드를 기동하고, 샤드당 정확히 1개의 팬인 포워더 goroutine을
// Execution/Snapshot 채널마다 띄운다. 포워더가 샤드당 1개인 것이 심볼 내 이벤트
// 순서 보존의 유일한 근거다 — 한 샤드 채널을 여러 goroutine이 경쟁 소비하면
// 그 심볼의 이벤트 순서가 뒤섞일 수 있다.
func (se *ShardedEngine) Start() {
	for _, shard := range se.shards {
		shard.Start()
	}

	var execWG sync.WaitGroup
	execWG.Add(len(se.shards))
	for _, shard := range se.shards {
		shard := shard
		go func() {
			defer execWG.Done()
			for ev := range shard.ExecutionCh {
				se.ExecutionCh <- ev
			}
		}()
	}

	var snapWG sync.WaitGroup
	snapWG.Add(len(se.shards))
	for _, shard := range se.shards {
		shard := shard
		go func() {
			defer snapWG.Done()
			for snap := range shard.SnapshotCh {
				se.SnapshotCh <- snap
			}
		}()
	}

	go func() {
		execWG.Wait()
		close(se.ExecutionCh)
	}()
	go func() {
		snapWG.Wait()
		close(se.SnapshotCh)
	}()
	go func() {
		execWG.Wait()
		snapWG.Wait()
		close(se.doneCh)
	}()
}

// SubmitOrder는 주문의 심볼이 소유한 샤드로 라우팅한다.
func (se *ShardedEngine) SubmitOrder(order *Order) {
	if order == nil {
		return
	}
	se.shardFor(order.CoinSymbol).SubmitOrder(order)
}

// CancelOrder는 취소 대상 심볼이 소유한 샤드로 라우팅한다 — 제출과 반드시 같은
// 샤드이므로 오더북 조회·제거가 일관된다.
func (se *ShardedEngine) CancelOrder(cmd CancelOrderCommand) CancelOrderResult {
	return se.shardFor(cmd.CoinSymbol).CancelOrder(cmd)
}

// RequestOrderBookSnapshot은 심볼이 소유한 샤드의 캐시에서 락 없이 읽는다.
func (se *ShardedEngine) RequestOrderBookSnapshot(coinSymbol string, depth int) (OrderBookSnapshot, error) {
	return se.shardFor(coinSymbol).RequestOrderBookSnapshot(coinSymbol, depth)
}

// SetMatchLatencyObserver는 전 샤드에 같은 옵저버를 설정한다.
func (se *ShardedEngine) SetMatchLatencyObserver(observer func(time.Duration)) {
	for _, shard := range se.shards {
		shard.MatchLatencyObserver = observer
	}
}

// Stop은 전 샤드에 종료를 지시한다. 각 샤드는 기존 동작대로 드레인 후 자기
// 채널을 닫고, Start()가 띄운 포워더들이 그 종료를 이어받아 머지 채널을 닫는다.
func (se *ShardedEngine) Stop() {
	se.stopOnce.Do(func() {
		for _, shard := range se.shards {
			shard.Stop()
		}
	})
}

// Done은 전 샤드의 포워더가 모두 종료되고(= 머지 채널이 닫히고) 나면 닫히는
// 채널을 반환한다.
func (se *ShardedEngine) Done() <-chan struct{} {
	return se.doneCh
}

// OrderChannelLen/CancelChannelLen/ExecutionChannelLen/SnapshotChannelLen은
// 기존 대시보드 호환용 샤드 합산 게이지에 쓰인다(main에서 이 메서드 값을
// func() int로 그대로 등록).
func (se *ShardedEngine) OrderChannelLen() int {
	total := 0
	for _, shard := range se.shards {
		total += len(shard.OrderCh)
	}
	return total
}

func (se *ShardedEngine) CancelChannelLen() int {
	total := 0
	for _, shard := range se.shards {
		total += len(shard.CancelCh)
	}
	return total
}

func (se *ShardedEngine) ExecutionChannelLen() int {
	total := 0
	for _, shard := range se.shards {
		total += len(shard.ExecutionCh)
	}
	return total
}

func (se *ShardedEngine) SnapshotChannelLen() int {
	total := 0
	for _, shard := range se.shards {
		total += len(shard.SnapshotCh)
	}
	return total
}

// ShardOrderChannelLens는 샤드별 order 채널 길이 접근자를 인덱스 순서대로
// 반환한다 — 샤드별 적체 가시성(신규 GaugeVec)을 위한 것이다. 20번 벤치마크가
// 채널 게이지로 단일 엔진의 병목을 잡아낸 선례를 따른다.
func (se *ShardedEngine) ShardOrderChannelLens() []func() int {
	lens := make([]func() int, len(se.shards))
	for i, shard := range se.shards {
		shard := shard
		lens[i] = func() int { return len(shard.OrderCh) }
	}
	return lens
}
