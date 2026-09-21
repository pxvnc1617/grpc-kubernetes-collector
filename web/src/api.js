// 수집 서버의 조회 API 를 감싼다.
//
// 개발 중에는 Vite 프록시를 타고, 빌드 후에는 같은 오리진이라
// 두 경우 모두 상대 경로로 부르면 된다.

async function get(path, params = {}) {
  const qs = new URLSearchParams(
    Object.entries(params).filter(([, v]) => v !== '' && v != null)
  ).toString()
  const res = await fetch(`/api${path}${qs ? `?${qs}` : ''}`)
  if (!res.ok) throw new Error(`${path} → ${res.status}`)
  return res.json()
}

export const api = {
  summary: () => get('/summary'),
  resources: (namespace = '', kind = '') => get('/resources', { namespace, kind }),
  metrics: (name = 'cpu_usage_millicores') => get('/metrics', { name }),
  series: (uid, name = 'cpu_usage_millicores') => get('/metrics/series', { uid, name }),
  logs: (namespace = '', level = '', limit = 200) => get('/logs', { namespace, level, limit }),
}
