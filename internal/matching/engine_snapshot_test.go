package matching

import (
	"testing"
	"time"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func drainSnapshotCh(me *MatchingEngine) []OrderBookSnapshot {
	var snaps []OrderBookSnapshot
	for {
		select {
		case s := <-me.SnapshotCh:
			snaps = append(snaps, s)
		default:
			return snaps
		}
	}
}

// flushSnapshots는 dirty 심볼당 스냅샷을 1회만 방출한다(코얼레싱). 같은 심볼을
// 여러 번 dirty로 표시해도 스냅샷은 1개다. flush 없이 직접 호출해 결정적으로 검증한다.
func TestFlushSnapshotsCoalescesDirtySymbols(t *testing.T) {
	me := NewMatchingEngine()
	me.markDirty("BTC")
	me.markDirty("BTC")
	me.markDirty("ETH")
	me.flushSnapshots()

	snaps := drainSnapshotCh(me)
	require.Len(t, snaps, 2, "dirty 심볼당 스냅샷 1개여야 한다(BTC 중복은 합쳐짐)")

	me.flushSnapshots()
	assert.Empty(t, drainSnapshotCh(me), "flush 후 dirty가 비어 재방출이 없어야 한다")
}

// flush는 캐시에도 저장하고, REST 조회는 그 캐시에서 읽는다(엔진 루프 미경유).
func TestFlushSnapshotsStoresToCacheForRESTRead(t *testing.T) {
	me := NewMatchingEngine()
	me.Match(testOrder(1, "BTC", model.OrderSideSell, 50000, 2))
	me.Match(testOrder(2, "BTC", model.OrderSideBuy, 40000, 3))
	me.markDirty("BTC")
	me.flushSnapshots()

	snap, err := me.RequestOrderBookSnapshot("BTC", DefaultSnapshotDepth)
	require.NoError(t, err)
	require.Len(t, snap.Asks, 1)
	require.Len(t, snap.Bids, 1)
	assert.True(t, snap.Asks[0].Price.Equal(decimal.NewFromInt(50000)))
	assert.True(t, snap.Bids[0].Price.Equal(decimal.NewFromInt(40000)))
}

func TestRequestSnapshotEmptyForUnknownSymbol(t *testing.T) {
	me := NewMatchingEngine()
	snap, err := me.RequestOrderBookSnapshot("NOPE", DefaultSnapshotDepth)
	require.NoError(t, err)
	assert.Equal(t, "NOPE", snap.CoinSymbol)
	assert.Empty(t, snap.Asks)
	assert.Empty(t, snap.Bids)
}

func TestRequestSnapshotClampsAndTruncatesDepth(t *testing.T) {
	me := NewMatchingEngine()
	for i := 0; i < 5; i++ {
		me.Match(testOrder(uint(i+1), "BTC", model.OrderSideSell, int64(50000+i*100), 1))
	}
	me.markDirty("BTC")
	me.flushSnapshots()

	snap, err := me.RequestOrderBookSnapshot("BTC", 2)
	require.NoError(t, err)
	assert.Len(t, snap.Asks, 2, "요청 depth 2로 잘려야 한다")

	// depth > DefaultSnapshotDepth 요청은 캐시 깊이(30)로 클램프된다(여기선 5개뿐이라 5개 전부).
	snap, err = me.RequestOrderBookSnapshot("BTC", 100)
	require.NoError(t, err)
	assert.Len(t, snap.Asks, 5)
}

// flush는 SnapshotCh가 가득 차도 블로킹되지 않아야 한다 — 오래된 스냅샷을 기다리며
// 엔진 루프를 막으면 안 된다. 전송은 skip되더라도 캐시 저장은 이뤄진다.
func TestFlushSnapshotsDoesNotBlockWhenChannelFull(t *testing.T) {
	me := NewMatchingEngine()
	for i := 0; i < cap(me.SnapshotCh); i++ {
		me.SnapshotCh <- OrderBookSnapshot{}
	}
	me.markDirty("BTC")

	done := make(chan struct{})
	go func() {
		me.flushSnapshots()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flushSnapshots가 SnapshotCh 가득 찬 상태에서 블로킹됐다")
	}

	snap, err := me.RequestOrderBookSnapshot("BTC", DefaultSnapshotDepth)
	require.NoError(t, err)
	assert.Equal(t, "BTC", snap.CoinSymbol, "전송이 skip돼도 캐시에는 저장돼야 한다")
}
