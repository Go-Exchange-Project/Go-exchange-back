import ws from 'k6/ws';
import { Counter } from 'k6/metrics';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const WS_URL = BASE_URL.replace(/^http/, 'ws') + '/ws';
const MODE = __ENV.MODE || 'legacy'; // legacy | subscribe
const N = parseInt(__ENV.N || '300', 10);
const DURATION = __ENV.DURATION || '9m30s';
const HOLD_MS = parseInt(__ENV.HOLD_MS || '570000', 10); // 9m30s
// subscribe 모드에서 구독할 심볼(콤마 구분). 기본 BTC 1개.
const SUBSCRIBE_SYMBOLS = (__ENV.SUBSCRIBE_SYMBOLS || 'BTC')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean);

const wsMessagesReceived = new Counter('ws_messages_received');
const wsConnected = new Counter('ws_connected_total');

export const options = {
  scenarios: {
    ws_load: {
      executor: 'constant-vus',
      vus: N,
      duration: DURATION,
      exec: 'wsClient',
    },
  },
};

// 서버 ServeWs는 Origin 체크가 있다 - 기본 허용 목록(http://localhost:3000)에 있는
// Origin을 명시해 서버 env 변경 없이 연결을 통과시킨다.
export function wsClient() {
  const params = { headers: { Origin: 'http://localhost:3000' } };
  const res = ws.connect(WS_URL, params, function (socket) {
    socket.on('open', function () {
      wsConnected.add(1);
      if (MODE === 'subscribe') {
        socket.send(JSON.stringify({ action: 'subscribe', coin_symbols: SUBSCRIBE_SYMBOLS }));
      }
    });
    socket.on('message', function () {
      wsMessagesReceived.add(1);
    });
    socket.setTimeout(function () {
      socket.close();
    }, HOLD_MS);
  });
  check(res, { 'ws connected (status 101)': (r) => r && r.status === 101 });
}
