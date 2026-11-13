// k6 smoke test for the public API on kind (M02 acceptance: p99 < 300 ms at 200 rps).
//
//   make load-smoke   (needs: make cluster kind-platform kind-deps deploy-services)
//
// setup() provisions everything through the real public API: a test-only password-grant client in Keycloak,
// USERS users with verified e-mails, one organization per user with a project, 20 resources and a read API key.
// The measured scenario then sends a constant RATE of read requests for DURATION, half with user tokens (JWT
// verification, cached edge authorization) and half with API keys (cached key resolution).
import http from 'k6/http';
import { check, fail } from 'k6';

const BASE = __ENV.BASE_URL || 'http://127.0.0.1:28080/v1';
const KC = __ENV.KC_URL || 'http://localhost:28180';
const USERS = parseInt(__ENV.USERS || '10', 10);
const RATE = parseInt(__ENV.RATE || '200', 10);
const DURATION = __ENV.DURATION || '60s';
const RUN = `${Date.now()}`;

export const options = {
  setupTimeout: '180s',
  scenarios: {
    smoke: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 50,
      maxVUs: 300,
    },
  },
  thresholds: {
    'http_req_duration{scenario:smoke}': ['p(99)<300', 'p(95)<150'],
    'http_req_failed{scenario:smoke}': ['rate<0.01'],
    'checks{scenario:smoke}': ['rate>0.99'],
    // The constant-arrival-rate executor must actually sustain the rate (no dropped iterations).
    'dropped_iterations': ['count<' + Math.ceil(RATE * 0.01 * 60)],
  },
};

function must(res, status, what) {
  if (res.status !== status) {
    fail(`${what}: expected ${status}, got ${res.status}: ${res.body && res.body.substring(0, 300)}`);
  }
  return res;
}

function admin(method, path, body, token) {
  const params = { headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' } };
  return http.request(method, `${KC}${path}`, body === null ? null : JSON.stringify(body), params);
}

function randomSecret() {
  let s = '';
  for (let i = 0; i < 4; i++) s += Math.random().toString(36).slice(2, 12);
  return s;
}

export function setup() {
  const adminToken = must(http.post(`${KC}/realms/master/protocol/openid-connect/token`, {
    grant_type: 'password', client_id: 'admin-cli', username: __ENV.KC_ADMIN_USER || 'admin', password: __ENV.KC_ADMIN_PASSWORD,
  }), 200, 'keycloak admin token').json('access_token');

  // Test-only confidential client with direct grants (never part of the committed realm).
  const clientSecret = randomSecret();
  let res = admin('POST', '/admin/realms/infrapilot/clients', {
    clientId: 'infrapilot-e2e', enabled: true, publicClient: false, secret: clientSecret, standardFlowEnabled: false,
    directAccessGrantsEnabled: true, fullScopeAllowed: false, defaultClientScopes: ['basic', 'profile', 'email', 'infrapilot-api'],
  }, adminToken);
  let secret = clientSecret;
  if (res.status === 409) {
    const id = must(admin('GET', '/admin/realms/infrapilot/clients?clientId=infrapilot-e2e', null, adminToken), 200, 'find client').json('0.id');
    secret = must(admin('POST', `/admin/realms/infrapilot/clients/${id}/client-secret`, null, adminToken), 200, 'rotate client secret').json('value');
  } else {
    must(res, 201, 'create test client');
  }

  const users = [];
  const keys = [];
  for (let i = 0; i < USERS; i++) {
    const name = `load-${i}`;
    const password = `Pw-${randomSecret()}`;
    res = admin('POST', '/admin/realms/infrapilot/users', {
      username: name, email: `${name}@example.com`, emailVerified: true, enabled: true, firstName: name, lastName: 'Load',
      credentials: [{ type: 'password', value: password, temporary: false }],
    }, adminToken);
    if (res.status === 409) {
      const id = must(admin('GET', `/admin/realms/infrapilot/users?username=${name}&exact=true`, null, adminToken), 200, 'find user').json('0.id');
      must(admin('PUT', `/admin/realms/infrapilot/users/${id}/reset-password`, { type: 'password', value: password, temporary: false }, adminToken), 204, 'reset password');
    } else {
      must(res, 201, 'create user');
    }
    const token = must(http.post(`${KC}/realms/infrapilot/protocol/openid-connect/token`, {
      grant_type: 'password', client_id: 'infrapilot-e2e', client_secret: secret, username: name, password, scope: 'openid',
    }), 200, 'user token').json('access_token');
    const auth = { headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' } };

    const org = must(http.post(`${BASE}/organizations`, JSON.stringify({ name: `Load ${i} ${RUN}` }), auth), 201, 'create org').json('id');
    const project = must(http.post(`${BASE}/organizations/${org}/projects`, JSON.stringify({ name: 'Load', slug: 'load' }), auth), 201, 'create project').json('id');
    const resources = [];
    for (let r = 0; r < 20; r++) {
      resources.push(must(http.post(`${BASE}/organizations/${org}/projects/${project}/resources`,
        JSON.stringify({ kind: 'host', name: `host-${r}`, labels: { env: 'load' } }), auth), 201, 'create resource').json('id'));
    }
    const key = must(http.post(`${BASE}/organizations/${org}/api-keys`,
      JSON.stringify({ project_id: project, name: 'load', scopes: ['read'] }), auth), 201, 'create api key').json('secret');
    users.push({ token, org, project, resources });
    keys.push({ key, org, project, resources });
  }
  return { users, keys };
}

export default function (data) {
  const useKey = Math.random() < 0.5;
  const pool = useKey ? data.keys : data.users;
  const u = pool[Math.floor(Math.random() * pool.length)];
  const headers = useKey ? { 'X-API-Key': u.key } : { Authorization: `Bearer ${u.token}` };
  const base = `${BASE}/organizations/${u.org}`;
  const resource = u.resources[Math.floor(Math.random() * u.resources.length)];
  const choice = Math.random();
  let res;
  let name;
  if (choice < 0.25) {
    name = 'get organization';
    res = http.get(base, { headers, tags: { name } });
  } else if (choice < 0.5) {
    name = 'list resources';
    res = http.get(`${base}/projects/${u.project}/resources?limit=50`, { headers, tags: { name } });
  } else if (choice < 0.75) {
    name = 'get resource';
    res = http.get(`${base}/projects/${u.project}/resources/${resource}`, { headers, tags: { name } });
  } else {
    name = 'get project';
    res = http.get(`${base}/projects/${u.project}`, { headers, tags: { name } });
  }
  check(res, { [`${name}: 200`]: (r) => r.status === 200 });
}
