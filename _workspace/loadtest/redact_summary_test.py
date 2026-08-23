#!/usr/bin/env python3
"""redact_summary.py 자동 테스트.

JWT fixture가 든 입력에서 세 가지를 고정한다:
  1. setup_data가 제거된다
  2. metrics는 불변이다
  3. 시크릿 스캔이 0건이 된다

의존성 없이 `python redact_summary_test.py`로 실행한다.
"""
from __future__ import annotations

import io
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import redact_summary as rs  # noqa: E402

# 실제 토큰이 아니다 — 형태만 같은 fixture다.
FAKE_JWT = (
    "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9."
    "eyJzdWIiOiJmaXh0dXJlIiwiaWF0IjowfQ."
    "ZmFrZS1zaWduYXR1cmUtZm9yLXRlc3Rpbmctb25seQ"
)

METRICS = {
    "sli_cancel_success": {"passes": 14813, "fails": 0, "value": 1},
    "http_req_duration": {"avg": 1.23, "p(95)": 4.56},
    "checks": {"passes": 209845, "fails": 0, "value": 1},
}


def make_summary(user_count: int) -> dict:
    return {
        "root_group": {"name": "", "checks": []},
        "metrics": json.loads(json.dumps(METRICS)),  # 깊은 복사
        "setup_data": {
            "users": [{"role": "maker", "token": FAKE_JWT} for _ in range(user_count)]
        },
    }


def write(path: str, doc: dict) -> None:
    with io.open(path, "w", encoding="utf-8", newline="\n") as handle:
        json.dump(doc, handle, ensure_ascii=False, indent=1)


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def test_redacts_and_keeps_metrics(tmp: str) -> None:
    path = os.path.join(tmp, "summary-a.json")
    write(path, make_summary(250))

    # JWT는 header와 payload가 모두 base64('{"...')라 `eyJ`로 시작한다 — 토큰 하나당
    # 패턴이 2회 잡힌다. 스캐너는 토큰 수가 아니라 패턴 출현 수를 센다.
    before_hits = rs.scan_file(path)
    check(before_hits["jwt"] == 500, f"fixture 250 토큰 = jwt 히트 500이어야 한다: {before_hits}")
    check(before_hits["token_field"] == 250, f"token 필드는 250이어야 한다: {before_hits}")

    count, error = rs.redact_file(path)
    check(error is None, f"정리가 실패했다: {error}")
    check(count == 250, f"제거 개수가 250이어야 한다: {count}")

    doc = rs.load_json(path)
    check(doc["setup_data"] == {"users_redacted": 250, "note": rs.REDACTION_NOTE},
          f"setup_data 치환 결과가 다르다: {doc['setup_data']}")
    check(doc["metrics"] == METRICS, "metrics가 바뀌었다")
    check(doc["root_group"] == {"name": "", "checks": []}, "metrics 외 필드가 바뀌었다")

    after_hits = rs.scan_file(path)
    check(not any(after_hits.values()), f"정리 후에도 히트가 남았다: {after_hits}")


def test_scan_reports_hits_without_values(tmp: str) -> None:
    path = os.path.join(tmp, "dirty.json")
    write(path, make_summary(3))

    hits = rs.scan_paths([path])
    check(path in hits, "히트가 보고되지 않았다")
    check(hits[path]["jwt"] == 6, f"3 토큰 = jwt 히트 6이어야 한다: {hits[path]}")
    # 값이 아니라 개수만 담아야 한다.
    for value in hits[path].values():
        check(isinstance(value, int), "스캔 결과에 값이 섞였다")


def test_exit_code_blocks_on_hit(tmp: str) -> None:
    clean = os.path.join(tmp, "clean.json")
    doc = make_summary(2)
    rs.redact_setup_data(doc)
    write(clean, doc)
    check(rs.main([clean]) == 0, "정리된 파일은 0을 반환해야 한다")

    dirty = os.path.join(tmp, "dirty2.json")
    write(dirty, make_summary(2))
    # --scan-only는 정리하지 않으므로 히트가 남아 2가 나와야 한다.
    check(rs.main(["--scan-only", dirty]) == 2, "히트가 있으면 2를 반환해야 한다")


def test_no_setup_data_is_not_an_error(tmp: str) -> None:
    path = os.path.join(tmp, "nosetup.json")
    write(path, {"metrics": json.loads(json.dumps(METRICS))})
    count, error = rs.redact_file(path)
    check(count is None and error is None, "setup_data가 없으면 조용히 건너뛰어야 한다")
    check(rs.load_json(path)["metrics"] == METRICS, "metrics가 바뀌었다")


def main() -> int:
    tests = [
        test_redacts_and_keeps_metrics,
        test_scan_reports_hits_without_values,
        test_exit_code_blocks_on_hit,
        test_no_setup_data_is_not_an_error,
    ]
    failed = 0
    with tempfile.TemporaryDirectory() as tmp:
        for test in tests:
            case = os.path.join(tmp, test.__name__)
            os.makedirs(case, exist_ok=True)
            try:
                test(case)
                print(f"  PASS {test.__name__}")
            except AssertionError as exc:
                failed += 1
                print(f"  FAIL {test.__name__}: {exc}")
    print(f"\n{len(tests) - failed}/{len(tests)} passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
