#!/usr/bin/env python3
"""k6 summary에서 setup_data(부하 사용자 JWT)를 제거하고 시크릿 스캔으로 통과 여부를 판정한다.

왜 필요한가:
    k6의 `--summary-export`는 `setup()`의 반환값을 `setup_data`에 통째로 덤프한다.
    이 저장소의 부하 하니스는 사용자별 JWT를 거기에 담으므로, 정리하지 않은 summary는
    토큰 수백~수천 개를 그대로 들고 있다. 34번과 35번에서 실제로 그렇게 남았다.

계약:
    - `metrics`를 포함한 지표 데이터는 절대 건드리지 않는다(정리 전후 파싱 비교로 검증).
    - `setup_data`는 사용자 수와 사유만 남긴 객체로 치환한다.
    - 시크릿 패턴이 하나라도 남으면 **exit 2로 실패**한다. 호출자는 packaging과 복사를
      중단해야 한다.

사용:
    python redact_summary.py <summary.json> [<summary.json> ...]
    python redact_summary.py --scan-only <path> [...]      # 디렉터리도 가능(재귀)

종료 코드:
    0  정리·검증 통과
    1  사용법·입출력 오류
    2  시크릿 스캔 히트 또는 metrics 불일치 — 진행 금지
"""
from __future__ import annotations

import argparse
import io
import json
import os
import re
import sys

REDACTION_NOTE = "per-user JWTs removed before packaging; only the count is kept"

# 값을 출력하지 않는다. 히트 개수와 종류만 보고한다.
SECRET_PATTERNS = {
    "jwt": re.compile(rb"eyJ[A-Za-z0-9_-]{8,}\."),
    "token_field": re.compile(rb'"token"\s*:'),
    "bearer": re.compile(rb"Bearer [A-Za-z0-9._-]{8,}"),
    "authorization": re.compile(rb"Authorization", re.IGNORECASE),
    "jwt_secret": re.compile(rb"GOEXCHANGE_JWT_SECRET"),
}


def load_json(path: str):
    with io.open(path, encoding="utf-8-sig") as handle:
        return json.load(handle)


def dump_json(path: str, doc) -> None:
    with io.open(path, "w", encoding="utf-8", newline="\n") as handle:
        json.dump(doc, handle, ensure_ascii=False, indent=1)
        handle.write("\n")


def redact_setup_data(doc) -> int | None:
    """setup_data.users를 개수로 치환한다. 대상이 없으면 None."""
    setup = doc.get("setup_data")
    if not isinstance(setup, dict) or "users" not in setup:
        return None
    count = len(setup.get("users") or [])
    doc["setup_data"] = {"users_redacted": count, "note": REDACTION_NOTE}
    return count


def scan_file(path: str) -> dict[str, int]:
    """파일 하나의 패턴별 히트 수. 값은 반환하지 않는다."""
    with open(path, "rb") as handle:
        blob = handle.read()
    return {name: len(pattern.findall(blob)) for name, pattern in SECRET_PATTERNS.items()}


def scan_paths(paths: list[str]) -> dict[str, dict[str, int]]:
    hits: dict[str, dict[str, int]] = {}
    for path in paths:
        targets = []
        if os.path.isdir(path):
            for root, _dirs, files in os.walk(path):
                targets.extend(os.path.join(root, name) for name in files)
        else:
            targets.append(path)
        for target in targets:
            counts = scan_file(target)
            if any(counts.values()):
                hits[target] = {k: v for k, v in counts.items() if v}
    return hits


def redact_file(path: str) -> tuple[int | None, str | None]:
    """(제거된 사용자 수, 오류) — metrics 불변을 확인한 뒤에만 기록한다."""
    before = load_json(path)
    metrics_before = before.get("metrics")

    count = redact_setup_data(before)
    if count is None:
        return None, None

    dump_json(path, before)

    after = load_json(path)
    if after.get("metrics") != metrics_before:
        return count, "metrics가 정리 전후로 달라졌다"
    return count, None


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("paths", nargs="+", help="summary JSON (또는 --scan-only 시 임의 경로)")
    parser.add_argument("--scan-only", action="store_true", help="정리하지 않고 스캔만 한다")
    args = parser.parse_args(argv)

    if not args.scan_only:
        for path in args.paths:
            if not os.path.isfile(path):
                print(f"[redact] 파일이 아니다: {path}", file=sys.stderr)
                return 1
            count, error = redact_file(path)
            if error:
                print(f"[redact] {path}: {error}", file=sys.stderr)
                return 2
            if count is None:
                print(f"[redact] {path}: setup_data 없음, 건너뜀")
            else:
                print(f"[redact] {path}: setup_data.users {count}개 제거, metrics 불변")

    hits = scan_paths(args.paths)
    if hits:
        print("[scan] 시크릿 패턴 히트, packaging/복사를 중단한다:", file=sys.stderr)
        for path, counts in sorted(hits.items()):
            detail = " ".join(f"{k}={v}" for k, v in sorted(counts.items()))
            print(f"  {path} | {detail}", file=sys.stderr)
        return 2

    print(f"[scan] 시크릿 히트 0건 ({len(args.paths)}개 경로)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
