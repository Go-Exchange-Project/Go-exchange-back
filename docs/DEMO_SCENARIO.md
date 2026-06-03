# Demo Scenario

이 문서는 로컬에서 Go Exchange MVP를 시연하는 순서를 정리합니다.

## 준비

백엔드 `.env.local` 예시:

```text
GOEXCHANGE_DB_PASSWORD=<local-postgres-password>
GOEXCHANGE_MARKET_RULES_PATH=config/market_rules.json
GOEXCHANGE_ENABLE_DEV_TOOLS=true
GOEXCHANGE_DEV_TOOLS_TOKEN=local-dev-token
GOEXCHANGE_ENABLE_UPBIT=false
GOEXCHANGE_CORS_ALLOWED_ORIGINS=http://localhost:3000,http://127.0.0.1:3000
GOEXCHANGE_WS_ALLOWED_ORIGINS=http://localhost:3000,http://127.0.0.1:3000
```

프론트엔드 `.env.local` 예시:

```text
VITE_API_BASE_URL=http://localhost:8080
VITE_WS_URL=ws://localhost:8080/ws
VITE_ENABLE_DEV_TOOLS=true
VITE_DEV_TOOLS_TOKEN=local-dev-token
```

서버 실행:

```powershell
cd C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-back
go run ./cmd
```

프론트 실행:

```powershell
cd C:\Users\dksco\OneDrive\Desktop\GoExchange\Go-exchange-front
npm run dev
```

브라우저에서 `http://localhost:3000`을 엽니다.

## 시나리오 1: 회원가입과 개발용 충전

1. 계정 A를 회원가입합니다.
2. `Fund BTC`로 BTC를 충전합니다.
3. 지갑 패널에서 BTC available이 증가했는지 확인합니다.
4. 계정 B를 회원가입하거나 로그아웃 후 다른 계정으로 가입합니다.
5. `Fund KRW`로 KRW를 충전합니다.
6. KRW available이 증가했는지 확인합니다.

확인 포인트:

- 로그인 후 protected API가 정상 호출됩니다.
- 개발용 fund는 dev token이 맞아야만 동작합니다.
- 지갑은 available/locked를 나눠 보여줍니다.

## 시나리오 2: 지정가 매도/매수 체결

1. 계정 A에서 BTC/KRW를 선택합니다.
2. Sell 탭에서 지정가 매도 주문을 냅니다.
   - 가격: `5000`
   - 수량: `1`
3. 계정 A의 BTC available이 줄고 locked가 증가하는지 확인합니다.
4. 계정 B에서 Buy 탭으로 이동합니다.
5. 지정가 매수 주문을 냅니다.
   - 가격: `5000`
   - 수량: `1`
6. 체결 후 계정 B의 BTC available이 `1` 증가하는지 확인합니다.
7. 계정 A의 KRW available이 체결대금에서 수수료를 뺀 만큼 증가하는지 확인합니다.

확인 포인트:

- 같은 가격에서 매수/매도가 체결됩니다.
- 매수자는 코인을 정확한 체결 수량만큼 받습니다.
- 수수료는 KRW로 부과됩니다.
- 주문 상태가 `FILLED`로 바뀝니다.
- 체결 내역에 buyer/seller fee가 표시됩니다.

## 시나리오 3: 주문 취소와 locked release

1. 계정 B에서 현재 체결되지 않을 낮은 가격의 지정가 매수 주문을 냅니다.
   - 가격: `5000`
   - 수량: `1`
2. KRW available이 줄고 locked가 증가하는지 확인합니다.
3. 열린 주문 목록에서 Cancel을 누릅니다.
4. KRW locked가 `0`이 되고 available이 원래대로 돌아오는지 확인합니다.

확인 포인트:

- 주문 생성 시 자산이 hold됩니다.
- 취소 시 남은 미체결 수량만 release됩니다.
- 중복 취소는 자산을 두 번 release하지 않습니다.

## 시나리오 4: 시장가 주문

시장가 매수:

1. 계정 A가 BTC 매도 호가를 올립니다.
2. 계정 B가 시장가 매수 주문을 냅니다.
3. 체결 후 남은 KRW 예산이 release되는지 확인합니다.

시장가 매도:

1. 계정 B가 BTC 매수 호가를 올립니다.
2. 계정 A가 시장가 매도 주문을 냅니다.
3. 시장가 주문이 오더북에 남지 않는지 확인합니다.

확인 포인트:

- 시장가 주문은 남은 수량/예산을 오더북에 rest하지 않습니다.
- 유동성이 없으면 시장가 주문은 취소되고 hold가 release됩니다.

## 시나리오 5: Self-trade 방지

1. 같은 계정으로 매도 주문을 냅니다.
2. 같은 계정으로 그 주문을 바로 살 수 있는 매수 주문을 냅니다.
3. 자기 주문끼리는 체결되지 않는지 확인합니다.
4. 다른 사용자의 반대 주문이 있으면 그 주문과는 체결되는지 확인합니다.

확인 포인트:

- 자기 주문은 matching 대상에서 제외됩니다.
- 자기 주문 때문에 다른 사람과의 정상 체결이 막히지 않습니다.

## 시나리오 6: 시장 정책 검증

1. tick size에 맞지 않는 가격으로 주문을 냅니다.
2. 최소 주문금액보다 작은 주문을 냅니다.
3. XRP처럼 정수 단위 수량 정책이 있는 코인에 소수 수량 주문을 냅니다.
4. HALT market에 주문을 냅니다.

확인 포인트:

- 잘못된 입력은 `422`나 `409`로 거부됩니다.
- 프론트 주문 폼에도 market rules가 반영됩니다.

## 시연 후 리셋

로컬 DB 데이터를 완전히 초기화하고 싶으면 개발 DB에서 테이블을 비우거나 새 DB를 만드는 것이 가장 안전합니다.
기존 open order가 남아 있으면 서버 재시작 시 matching bootstrap으로 다시 오더북에 올라옵니다.
