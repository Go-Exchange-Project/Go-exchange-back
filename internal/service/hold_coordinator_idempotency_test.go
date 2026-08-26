package service

import (
	"testing"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func idemReq(userID uint, key, fingerprint string) holdRequest {
	return holdRequest{
		order: &model.Order{UserID: userID},
		idem:  &idempotencyContext{Key: key, Fingerprint: fingerprint, Version: CurrentOrderFingerprintVersion},
	}
}

// 같은 키·같은 지문이면 하나가 owner, 나머지는 follower다. 둘 다 owner가 되면
// hold는 한 번인데 엔진 제출이 두 번이 된다.
func TestGroupIdempotentRequestsAssignsOneOwnerPerKey(t *testing.T) {
	reqs := []holdRequest{
		idemReq(1, "k1", "fp"),
		idemReq(1, "k1", "fp"),
		idemReq(2, "k1", "fp"), // 다른 사용자 — 별개다
	}

	owners, followers, conflicts := groupIdempotentRequests(reqs)

	assert.Equal(t, []int{0, 2}, owners)
	assert.Equal(t, map[int]int{1: 0}, followers, "인덱스 1은 0을 따라야 한다")
	assert.Empty(t, conflicts)
}

// 같은 키·다른 지문이면 하나만 진행하고 나머지는 409다.
func TestGroupIdempotentRequestsMarksFingerprintConflicts(t *testing.T) {
	reqs := []holdRequest{
		idemReq(1, "k1", "fp-a"),
		idemReq(1, "k1", "fp-b"),
	}

	owners, followers, conflicts := groupIdempotentRequests(reqs)

	assert.Equal(t, []int{0}, owners)
	assert.Empty(t, followers)
	assert.Equal(t, []int{1}, conflicts)
}

// map 순회 순서에 맡기면 같은 입력이 실행마다 다른 결과를 낸다.
func TestGroupIdempotentRequestsIsDeterministic(t *testing.T) {
	build := func() []holdRequest {
		return []holdRequest{
			idemReq(1, "k1", "fp-a"),
			idemReq(1, "k1", "fp-b"),
			idemReq(1, "k2", "fp-c"),
			idemReq(2, "k1", "fp-d"),
		}
	}

	owners, followers, conflicts := groupIdempotentRequests(build())
	for i := 0; i < 50; i++ {
		o, f, c := groupIdempotentRequests(build())
		require.Equal(t, owners, o, "%d회차 owner가 달라졌다", i)
		require.Equal(t, followers, f, "%d회차 follower가 달라졌다", i)
		require.Equal(t, conflicts, c, "%d회차 conflict가 달라졌다", i)
	}
}

// 키 없는 요청은 그룹화 대상이 아니다 — 전부 owner로 남아야 한다.
func TestGroupIdempotentRequestsKeepsUnkeyedRequestsAsOwners(t *testing.T) {
	reqs := []holdRequest{
		{order: &model.Order{UserID: 1}},
		{order: &model.Order{UserID: 1}},
	}

	owners, followers, conflicts := groupIdempotentRequests(reqs)

	assert.Equal(t, []int{0, 1}, owners)
	assert.Empty(t, followers)
	assert.Empty(t, conflicts)
}
