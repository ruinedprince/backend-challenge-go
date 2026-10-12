import http from 'k6/http';
import { check, sleep } from 'k6';
import encoding from 'k6/encoding';

export const options = {
  vus: Number(__ENV.VUS || 6),
  duration: __ENV.DURATION || '15s',
  thresholds: { http_req_failed: ['rate<0.01'], http_req_duration: ['p(95)<1000'] },
};

const keycloak = __ENV.KEYCLOAK_URL || 'http://keycloak:8080';
const apis = (__ENV.API_URLS || 'http://api-a:8080,http://api-b:8080,http://api-c:8080').split(',');

function token(client, secret) {
  const response = http.post(`${keycloak}/realms/wagering/protocol/openid-connect/token`, 'grant_type=client_credentials', {
    headers: { Authorization: `Basic ${encoding.b64encode(`${client}:${secret}`)}`, 'Content-Type': 'application/x-www-form-urlencoded' },
  });
  check(response, { 'token issued': (r) => r.status === 200 });
  return response.json('access_token');
}

export function setup() {
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
  return { providerToken, wallets };
}

export default function (data) {
  const wallet = data.wallets[(__VU - 1) % data.wallets.length];
  const externalID = `load-${__VU}-${__ITER}-${Date.now()}`;
  const response = http.post(`${apis[(__VU + __ITER) % apis.length]}/wagering/transactions`, JSON.stringify({
    providerId: 'provider-a', externalTransactionId: externalID, playerId: wallet.playerId, walletId: wallet.id,
    roundId: `round-${__VU}`, gameId: 'load-test', kind: 'BET', money: { amount: '1.00', currency: 'BRL' },
  }), { headers: { Authorization: `Bearer ${data.providerToken}`, 'Idempotency-Key': `provider-a:${externalID}`, 'Content-Type': 'application/json' } });
  check(response, { 'transaction accepted': (r) => r.status === 200, 'transaction processed': (r) => r.json('status') === 'PROCESSED' });
  sleep(0.1);
}
