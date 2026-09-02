// quantumselect는 보존된 baseline과 후보 측정 JSON을 읽어 사전 등록된
// 규칙으로 매칭 quantum 값을 고른다.
//
// 일회성 스크립트가 아니라 커밋되는 프로그램이어야 판정이 재현된다.
//
//	go run ./cmd/quantumselect -baseline _workspace/quantum/baseline -baseline-runs 3 \
//	    -candidates _workspace/quantum/explore -runs 3 \
//	    -semantic _workspace/quantum/semantic.json -top 2
//
// 통과 조합이 0개면 exit 2로 끝난다. 그 경우 임계값을 완화하거나 격자를
// 넓히지 않고 중단·보고한다.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Go-Exchange-Project/Go-exchange-back/internal/matching"
)

func loadRuns(dir string) ([]matching.RunFile, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var all []matching.RunFile
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var runs []matching.RunFile
		if err := json.Unmarshal(data, &runs); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		all = append(all, runs...)
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%s: no run files", dir)
	}
	return all, nil
}

// parseComboDir는 "m32-c8" 형태의 디렉터리명에서 설정을 읽는다.
func parseComboDir(name string) (matching.QuantumConfig, error) {
	parts := strings.Split(name, "-")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "m") || !strings.HasPrefix(parts[1], "c") {
		return matching.QuantumConfig{}, fmt.Errorf("bad combo dir %q, want m<N>-c<N>", name)
	}
	m, err := strconv.Atoi(parts[0][1:])
	if err != nil {
		return matching.QuantumConfig{}, fmt.Errorf("bad combo dir %q: %w", name, err)
	}
	c, err := strconv.Atoi(parts[1][1:])
	if err != nil {
		return matching.QuantumConfig{}, fmt.Errorf("bad combo dir %q: %w", name, err)
	}
	return matching.QuantumConfig{MaxMatchesPerTurn: m, MaxConsecutiveCancels: c}, nil
}

func main() {
	baselineDir := flag.String("baseline", "", "보존된 baseline JSON 디렉터리")
	baselineRuns := flag.Int("baseline-runs", 3, "baseline 시나리오당 회차 수")
	candidatesDir := flag.String("candidates", "", "후보별 하위 디렉터리를 담은 디렉터리")
	runs := flag.Int("runs", 3, "후보 시나리오당 회차 수")
	semanticPath := flag.String("semantic", "", "조합별 의미 테스트 통과 여부 JSON")
	top := flag.Int("top", 1, "상위 N개 출력 (1차 탐색은 2, 확증은 1)")
	flag.Parse()

	// baseline도 후보 선택 전에 실제 parser로 읽어 검증한다.
	baseRuns, err := loadRuns(*baselineDir)
	must(err)
	base, err := matching.AggregateBaseline(baseRuns, *baselineRuns)
	must(err)
	fmt.Printf("baseline  H0=%v  H2-1=%v  H2-5000=%v\n\n",
		base.H0OrderP99, base.H2SmallCancelP99, base.H2LargeSweepTotal)

	semanticData, err := os.ReadFile(*semanticPath)
	must(err)
	var semantic map[string]bool
	must(json.Unmarshal(semanticData, &semantic))

	entries, err := os.ReadDir(*candidatesDir)
	must(err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// 격자 순서를 결정론적으로 만든다 — 규칙 3(입력 순서)이 재현되려면
	// 디렉터리 나열 순서에 의존하면 안 된다.
	sort.Slice(names, func(i, j int) bool {
		a, _ := parseComboDir(names[i])
		b, _ := parseComboDir(names[j])
		if a.MaxMatchesPerTurn != b.MaxMatchesPerTurn {
			return a.MaxMatchesPerTurn < b.MaxMatchesPerTurn
		}
		return a.MaxConsecutiveCancels < b.MaxConsecutiveCancels
	})

	candidates := make([]matching.CandidateResult, 0, len(names))
	for _, name := range names {
		cfg, err := parseComboDir(name)
		must(err)
		runFiles, err := loadRuns(filepath.Join(*candidatesDir, name))
		must(err)
		result, err := matching.AggregateCandidate(cfg, runFiles, *runs, semantic[name])
		must(err)
		candidates = append(candidates, result)

		verdict := result.FailedGate(base)
		if verdict == "" {
			verdict = "PASS"
		}
		fmt.Printf("%-10s %-26s censored=%-3d semantic=%-5v H1=%-12v H0=%-12v ratio=%-7.1f cancel=%-12v sweep=%-12v gap=%v\n",
			name, verdict, result.Censored, semantic[name],
			result.H1OrderP99, result.H0OrderP99, result.StarvationRatio(),
			result.H2LargeCancelP99, result.H2LargeSweepTotal, result.MaxSnapshotGap)
	}

	ranked := matching.RankCandidates(candidates, base)
	if len(ranked) == 0 {
		fmt.Fprintf(os.Stderr, "\nSELECTION FAILED: %v\n", matching.ErrNoCandidatePassed)
		fmt.Fprintln(os.Stderr, "임계값을 완화하거나 격자를 넓히지 말고 P1으로 중단할 것.")
		os.Exit(2)
	}
	fmt.Printf("\nPASSED %d/%d\n", len(ranked), len(candidates))
	for i, c := range ranked {
		if i >= *top {
			break
		}
		fmt.Printf("RANK%d m%d-c%d\n", i+1, c.Config.MaxMatchesPerTurn, c.Config.MaxConsecutiveCancels)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
