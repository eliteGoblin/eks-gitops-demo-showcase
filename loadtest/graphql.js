// k6 load test for demo-api's GraphQL endpoint: a realistic read mix from a
// demo participant, ramped over ~3 minutes, to show the HPA scaling pods.
//
//   k6 run -e BASE_URL=https://app.frankbluemind.com \
//          -e TOKEN_URL=https://auth.frankbluemind.com/realms/demo/protocol/openid-connect/token \
//          -e CLIENT_SECRET=... loadtest/graphql.js
//
// The token comes from the load-generator client (client credentials); its
// service account is a participant, like demo.guest. Tokens last 5 minutes,
// longer than the run. Env: BASE_URL, TOKEN_URL, CLIENT_ID, CLIENT_SECRET,
// PEAK_VUS (default 20), PEAK_DURATION (default 2m), PARTICIPANT_ID (default P3), PLAN_ID (default PL3),
// WRITE_RATE (default 0.002: the share of iterations that raise a support request).
import http from 'k6/http';
import { check, fail, sleep } from 'k6';
import { Counter } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8082';
const TOKEN_URL = __ENV.TOKEN_URL || 'http://localhost:8080/realms/demo/protocol/openid-connect/token';
const CLIENT_ID = __ENV.CLIENT_ID || 'load-generator';
const CLIENT_SECRET = __ENV.CLIENT_SECRET || 'dev-only-load-generator-secret';
const PEAK = parseInt(__ENV.PEAK_VUS || '20', 10);
const PEAK_DURATION = __ENV.PEAK_DURATION || '2m'; // how long to hold the peak
const PARTICIPANT_ID = __ENV.PARTICIPANT_ID || 'P3';
const PLAN_ID = __ENV.PLAN_ID || 'PL3';

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-vus',
      startVUs: 1,
      stages: [
        { duration: '30s', target: Math.ceil(PEAK / 2) },
        { duration: PEAK_DURATION, target: PEAK },
        { duration: '30s', target: 0 },
      ],
      gracefulRampDown: '10s',
    },
  },
  thresholds: {
    // 429 is the API's rate limiter doing its job, so it is not a failure;
    // anything else outside 200 is.
    http_req_failed: ['rate<0.01'],
    'checks{kind:graphql}': ['rate>0.99'],
    http_req_duration: ['p(95)<800'],
  },
};

http.setResponseCallback(http.expectedStatuses(200, 429));
const rateLimited = new Counter('rate_limited');

const QUERIES = [
  { weight: 3, name: 'me', query: '{ me { subject roles participant { id givenName } } }' },
  {
    weight: 4, name: 'participant',
    query: `{ participant(id: "${PARTICIPANT_ID}") { id givenName dateOfBirth plans { id status budgets { category allocated spent remaining } } } }`,
  },
  {
    weight: 3, name: 'plan_claims',
    query: `{ plan(id: "${PLAN_ID}") { id claims(first: 10) { totalCount edges { node { id status amount provider { name } } } } } }`,
  },
];
const TOTAL_WEIGHT = QUERIES.reduce((sum, q) => sum + q.weight, 0);

// A trickle of writes (about 1 in 500 iterations, ~30 per run): the demo
// participant raising a small support request on its own plan, so the load
// exercises the transactional path too without flooding the staff queue.
const WRITE_RATE = parseFloat(__ENV.WRITE_RATE || '0.002');
const RAISE = `mutation { raiseSupportRequest(input: {planId: "${PLAN_ID}", category: THERAPY, description: "load test", estimatedAmount: "1.00"}) { __typename ... on SupportRequest { id status } ... on ValidationError { field message } } }`;

function pick() {
  let r = Math.random() * TOTAL_WEIGHT;
  for (const q of QUERIES) {
    if ((r -= q.weight) < 0) return q;
  }
  return QUERIES[0];
}

export function setup() {
  const res = http.post(TOKEN_URL, {
    grant_type: 'client_credentials', client_id: CLIENT_ID, client_secret: CLIENT_SECRET,
  });
  if (res.status !== 200) fail(`token request failed: HTTP ${res.status} ${res.body}`);
  return { token: res.json('access_token') };
}

export default function (data) {
  const write = Math.random() < WRITE_RATE;
  const q = write ? { name: 'raise_support_request', query: RAISE } : pick();
  const res = http.post(`${BASE_URL}/graphql`, JSON.stringify({ query: q.query }), {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${data.token}` },
    tags: { op: q.name },
  });
  if (res.status === 429) {
    rateLimited.add(1);
    sleep(1); // back off like a well-behaved client
    return;
  }
  check(res, {
    'status 200': (r) => r.status === 200,
    'no GraphQL errors': (r) => !r.json('errors'),
  }, { kind: 'graphql' });
  sleep(0.2 + Math.random() * 0.3); // think time: ~2-5 requests/s per virtual user
}
