import http from 'k6/http';
import { check, sleep } from 'k6';
import encoding from 'k6/encoding';
import { Trend } from 'k6/metrics';

// These focus the report on the actual wager endpoint rather than token,
// wallet, or observability requests made by the scenario itself.
const wagerDuration = new Trend('wager_transaction_duration', true);
const observedOutboxDelay = new Trend('wager_outbox_pending_delay_seconds', false);
const observedConcurrencyConflicts = new Trend('wager_concurrency_conflicts_during_run', false);

export const options = {
  vus: Number(__ENV.VUS || 6),
  duration: __ENV.DURATION || '15s',
  thresholds: { http_req_failed: ['rate<0.01'], http_req_duration: ['p(95)<1000'] },
  summaryTrendStats: ['avg', 'min', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

const keycloak = __ENV.KEYCLOAK_URL || 'http://keycloak:8080';
const apis = (__ENV.API_URLS || 'http://api-a:8080,http://api-b:8080,http://api-c:8080').split(',');

function metricValue(metrics, name) {
  const line = metrics.split('\n').find((candidate) => candidate.startsWith(`${name} `));
  const values = line ? line.split(' ') : [];
  return values.length > 0 ? Number(values[values.length - 1]) : 0;
}

function operationalSnapshot() {
  return apis.reduce((snapshot, api) => {
    const response = http.get(`${api}/metrics`);
    const metrics = response.body || '';
    return {
      concurrencyConflicts: snapshot.concurrencyConflicts + metricValue(metrics, 'wager_concurrency_conflicts_total'),
      oldestOutboxDelay: Math.max(snapshot.oldestOutboxDelay, metricValue(metrics, 'wager_outbox_oldest_pending_seconds')),
    };
  }, { concurrencyConflicts: 0, oldestOutboxDelay: 0 });
}

function token(client, secret) {
  const response = http.post(`${keycloak}/realms/wagering/protocol/openid-connect/token`, 'grant_type=client_credentials', {
    headers: { Authorization: `Basic ${encoding.b64encode(`${client}:${secret}`)}`, 'Content-Type': 'application/x-www-form-urlencoded' },
  });
  check(response, { 'token issued': (r) => r.status === 200 });
  return response.json('access_token');
}

export function setup() {
  const initialOperations = operationalSnapshot();
  const internalToken = token('wallet-service', 'wallet-service-secret');
  const providerToken = token('provider-a', 'provider-a-secret');
  const wallets = [];
  for (let index = 0; index < options.vus; index++) {
    const response = http.post(`${apis[index % apis.length]}/wallets`, JSON.stringify({
      providerId: 'provider-a', playerId: `00000000-0000-0000-0000-${String(Date.now() + index).slice(-12)}`,
      initialBalance: { amount: '1000.00', currency: 'BRL' },
    }), { headers: { Authorization: `Bearer ${internalToken}`, 'Content-Type': 'application/json' } });
    check(response, { 'wallet opened': (r) => r.status === 201 });
    wallets.push(response.json());
  }
  return { providerToken, wallets, initialOperations };
}

export default function (data) {
  const wallet = data.wallets[(__VU - 1) % data.wallets.length];
  const externalID = `load-${__VU}-${__ITER}-${Date.now()}`;
  const response = http.post(`${apis[(__VU + __ITER) % apis.length]}/wagering/transactions`, JSON.stringify({
    providerId: 'provider-a', externalTransactionId: externalID, playerId: wallet.playerId, walletId: wallet.id,
    roundId: `round-${__VU}`, gameId: 'load-test', kind: 'BET', money: { amount: '1.00', currency: 'BRL' },
  }), { headers: { Authorization: `Bearer ${data.providerToken}`, 'Idempotency-Key': `provider-a:${externalID}`, 'Content-Type': 'application/json' } });
  wagerDuration.add(response.timings.duration);
  check(response, { 'transaction accepted': (r) => r.status === 200, 'transaction processed': (r) => r.json('status') === 'PROCESSED' });

  // One VU samples shared operational state periodically without distorting
  // the load profile with a metrics request on every wager.
  if (__VU === 1 && __ITER % 5 === 0) {
    const operations = operationalSnapshot();
    observedOutboxDelay.add(operations.oldestOutboxDelay);
    observedConcurrencyConflicts.add(operations.concurrencyConflicts - data.initialOperations.concurrencyConflicts);
  }
  sleep(0.1);
}

export function teardown(data) {
  const operations = operationalSnapshot();
  const conflicts = operations.concurrencyConflicts - data.initialOperations.concurrencyConflicts;
  console.log(`Observability summary: concurrency conflicts during run=${conflicts}; oldest pending outbox event=${operations.oldestOutboxDelay.toFixed(3)}s`);
}
