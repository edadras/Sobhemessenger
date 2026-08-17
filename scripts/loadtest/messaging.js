/*
 * SOBH load test (§78, §79).
 *
 * Run with k6:
 *
 *   k6 run -e BASE_URL=https://staging.sobh.app scripts/loadtest/messaging.js
 *   k6 run -e STAGE=50k scripts/loadtest/messaging.js
 *
 * The thresholds below are the specification's targets, not aspirations: the
 * run fails if p95 API latency exceeds 200ms, message acknowledgement exceeds
 * 300ms, or WebSocket delivery exceeds 500ms. A load test that cannot fail is
 * only a traffic generator.
 */

import http from 'k6/http';
import ws from 'k6/ws';
import { check, sleep, fail } from 'k6';
import { Trend, Rate, Counter } from 'k6/metrics';
import { randomIntBetween, uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const WS_URL = __ENV.WS_URL || BASE_URL.replace(/^http/, 'ws') + '/ws';
const STAGE = __ENV.STAGE || '10k';

/* Custom metrics that map directly onto the §79 targets. */
const sendAckLatency = new Trend('sobh_send_ack_ms', true);
const wsDeliveryLatency = new Trend('sobh_ws_delivery_ms', true);
const wsConnectRate = new Rate('sobh_ws_connect_success');
const messagesSent = new Counter('sobh_messages_sent');
const messagesReceived = new Counter('sobh_messages_received');

/*
 * The ramp profile from §78. Each stage is a separate run rather than one long
 * ramp, so a failure at 50k does not hide behind an average that includes the
 * gentler 10k phase.
 */
const profiles = {
  '10k': [
    { duration: '2m', target: 1000 },
    { duration: '5m', target: 5000 },
    { duration: '10m', target: 10000 },
    { duration: '5m', target: 10000 },
    { duration: '2m', target: 0 },
  ],
  '50k': [
    { duration: '5m', target: 10000 },
    { duration: '10m', target: 30000 },
    { duration: '15m', target: 50000 },
    { duration: '10m', target: 50000 },
    { duration: '5m', target: 0 },
  ],
  '100k': [
    { duration: '10m', target: 25000 },
    { duration: '15m', target: 60000 },
    { duration: '20m', target: 100000 },
    { duration: '15m', target: 100000 },
    { duration: '5m', target: 0 },
  ],
  smoke: [
    { duration: '30s', target: 20 },
    { duration: '1m', target: 20 },
    { duration: '15s', target: 0 },
  ],
};

export const options = {
  scenarios: {
    messaging: {
      executor: 'ramping-vus',
      stages: profiles[STAGE] || profiles.smoke,
      gracefulRampDown: '30s',
    },
  },
  thresholds: {
    /* §79: API p95 under 200ms. */
    'http_req_duration{expected_response:true}': ['p(95)<200'],
    /* §79: send acknowledgement under 300ms. */
    sobh_send_ack_ms: ['p(95)<300'],
    /* §79: WebSocket delivery under 500ms. */
    sobh_ws_delivery_ms: ['p(95)<500'],
    /* A connection failure rate above 1% means the fan-out is not holding. */
    sobh_ws_connect_success: ['rate>0.99'],
    http_req_failed: ['rate<0.01'],
  },
  /* Staging certificates are often self-signed; production runs should not
     need this and it is deliberately opt-in. */
  insecureSkipTLSVerify: __ENV.INSECURE_TLS === 'true',
};

/*
 * setup provisions one account per virtual-user slot before the ramp starts, so
 * the measured phase contains no registration traffic.
 *
 * It relies on SMS_ECHO_CODES being enabled, which configuration permits only
 * outside production — a load test must never be pointed at real users.
 */
export function setup() {
  const accounts = [];
  const poolSize = Number(__ENV.ACCOUNT_POOL || 50);

  for (let i = 0; i < poolSize; i++) {
    const phone = `+9891${String(100000000 + i).slice(0, 9)}`;

    const requested = http.post(
      `${BASE_URL}/api/v1/auth/otp/request`,
      JSON.stringify({ phone }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'otp_request' } },
    );
    if (requested.status !== 200) {
      continue;
    }

    const code = requested.json('data.debug_code');
    if (!code) {
      fail('SMS_ECHO_CODES must be enabled on the target environment to seed accounts');
    }

    const verified = http.post(
      `${BASE_URL}/api/v1/auth/otp/verify`,
      JSON.stringify({
        phone,
        code,
        device_name: 'k6',
        platform: 'android',
        app_version: 'loadtest',
      }),
      { headers: { 'Content-Type': 'application/json' }, tags: { name: 'otp_verify' } },
    );
    if (verified.status !== 200) {
      continue;
    }

    accounts.push({
      userId: verified.json('data.user_id'),
      token: verified.json('data.access_token'),
    });
  }

  if (accounts.length < 2) {
    fail(`seeded only ${accounts.length} accounts; at least 2 are required`);
  }

  /* Pair each account with a peer and open the chat once, so the measured
     phase never pays for chat creation. */
  const chats = [];
  for (let i = 0; i + 1 < accounts.length; i += 2) {
    const opened = http.post(
      `${BASE_URL}/api/v1/chats/private`,
      JSON.stringify({ user_id: accounts[i + 1].userId }),
      {
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${accounts[i].token}`,
        },
        tags: { name: 'open_chat' },
      },
    );
    if (opened.status === 200) {
      chats.push({
        chatId: opened.json('data.chat_id'),
        sender: accounts[i],
        receiver: accounts[i + 1],
      });
    }
  }

  return { chats };
}

export default function (data) {
  if (!data.chats || data.chats.length === 0) {
    fail('setup produced no chats');
  }

  const pair = data.chats[__VU % data.chats.length];
  const authHeaders = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${pair.sender.token}`,
  };

  /*
   * A quarter of the virtual users hold a WebSocket and measure delivery
   * latency; the rest exercise the REST path. Mixing them in one run is what
   * makes the numbers representative — a real deployment carries both.
   */
  if (__VU % 4 === 0) {
    exerciseWebSocket(pair);
  } else {
    exerciseRest(pair, authHeaders);
  }
}

function exerciseRest(pair, headers) {
  /* Chat list: the most frequent read in the product. */
  const list = http.get(`${BASE_URL}/api/v1/chats?limit=20`, {
    headers,
    tags: { name: 'list_chats' },
  });
  check(list, { 'chat list is 200': (r) => r.status === 200 });

  /* Send, and measure the round trip against the 300ms target. */
  const started = Date.now();
  const sent = http.post(
    `${BASE_URL}/api/v1/chats/${pair.chatId}/messages`,
    JSON.stringify({
      client_message_id: uuidv4(),
      type: 'text',
      content: `load test ${randomIntBetween(1, 1000000)}`,
    }),
    { headers, tags: { name: 'send_message' } },
  );

  if (sent.status === 201) {
    sendAckLatency.add(Date.now() - started);
    messagesSent.add(1);
  }
  check(sent, { 'send is 201': (r) => r.status === 201 });

  /* Read history, which is the other dominant query. */
  const history = http.get(
    `${BASE_URL}/api/v1/chats/${pair.chatId}/messages?limit=50`,
    { headers, tags: { name: 'read_history' } },
  );
  check(history, { 'history is 200': (r) => r.status === 200 });

  /* Real users pause between actions; hammering without think time measures
     the load generator rather than the product. */
  sleep(randomIntBetween(1, 4));
}

function exerciseWebSocket(pair) {
  /*
   * The socket is held by the receiver and the messages are sent by the other
   * party over REST. That asymmetry is deliberate: the server does not echo a
   * message back to its own sender, so a virtual user that both sends and
   * listens on one socket would measure acknowledgement and never delivery.
   */
  const url = `${WS_URL}?token=${pair.receiver.token}&protocol_version=1`;
  const senderHeaders = {
    'Content-Type': 'application/json',
    Authorization: `Bearer ${pair.sender.token}`,
  };

  const response = ws.connect(url, {}, function (socket) {
    socket.on('open', function () {
      wsConnectRate.add(true);

      socket.setInterval(function () {
        const started = Date.now();
        const sent = http.post(
          `${BASE_URL}/api/v1/chats/${pair.chatId}/messages`,
          JSON.stringify({
            client_message_id: uuidv4(),
            type: 'text',
            content: `ws load ${randomIntBetween(1, 1000000)}`,
          }),
          { headers: senderHeaders, tags: { name: 'send_for_delivery' } },
        );

        if (sent.status === 201) {
          sendAckLatency.add(Date.now() - started);
          messagesSent.add(1);
        }
      }, randomIntBetween(2000, 6000));

      /* Application-level heartbeat, mirroring what the app does. */
      socket.setInterval(function () {
        socket.send(JSON.stringify({ event: 'ping', payload: {} }));
      }, 25000);
    });

    socket.on('message', function (raw) {
      let frame;
      try {
        frame = JSON.parse(raw);
      } catch (error) {
        return;
      }

      if (frame.event === 'message.new') {
        messagesReceived.add(1);
        /* created_at is server-assigned, so this measures the real path from
           persistence to this client rather than clock skew alone. */
        const createdAt = frame.payload && frame.payload.message
          ? Date.parse(frame.payload.message.created_at)
          : null;
        if (createdAt) {
          wsDeliveryLatency.add(Date.now() - createdAt);
        }
      }
    });

    socket.on('error', function () {
      wsConnectRate.add(false);
    });

    /* Hold the connection for a realistic session length rather than
       connecting and dropping, which would never exercise fan-out. */
    socket.setTimeout(function () {
      socket.close();
    }, randomIntBetween(30000, 90000));
  });

  check(response, { 'websocket handshake is 101': (r) => r && r.status === 101 });
}

export function handleSummary(data) {
  const p95 = (metric) =>
    data.metrics[metric] && data.metrics[metric].values
      ? Math.round(data.metrics[metric].values['p(95)'] || 0)
      : 0;

  const summary = [
    '',
    `SOBH load test — stage ${STAGE}`,
    '─'.repeat(48),
    `API p95            ${p95('http_req_duration')} ms   (target < 200)`,
    `Send ack p95       ${p95('sobh_send_ack_ms')} ms   (target < 300)`,
    `WS delivery p95    ${p95('sobh_ws_delivery_ms')} ms   (target < 500)`,
    `Messages sent      ${data.metrics.sobh_messages_sent?.values?.count || 0}`,
    `Messages received  ${data.metrics.sobh_messages_received?.values?.count || 0}`,
    '',
  ].join('\n');

  return {
    stdout: summary,
    'loadtest-summary.json': JSON.stringify(data, null, 2),
  };
}
