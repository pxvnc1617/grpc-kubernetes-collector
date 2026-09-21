<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from './api'
import StatCard from './components/StatCard.vue'
import ResourceTable from './components/ResourceTable.vue'
import MetricPanel from './components/MetricPanel.vue'
import LogPanel from './components/LogPanel.vue'

const summary = ref(null)
const resources = ref([])
const cpu = ref([])
const mem = ref([])
const logs = ref([])

const error = ref('')
const lastAt = ref(null)
const loading = ref(true)

let timer = null

async function refresh() {
  try {
    const [s, r, c, m, l] = await Promise.all([
      api.summary(),
      api.resources(),
      api.metrics('cpu_usage_millicores'),
      api.metrics('memory_usage_bytes'),
      api.logs('', '', 300),
    ])
    summary.value = s
    resources.value = r ?? []
    cpu.value = c ?? []
    mem.value = m ?? []
    logs.value = l ?? []
    lastAt.value = new Date()
    error.value = ''
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  refresh()
  timer = setInterval(refresh, 5000)
})
onUnmounted(() => clearInterval(timer))

const rel = computed(() => summary.value?.relations ?? { total: 0, resolved: 0 })
const stats = computed(() => summary.value?.stats ?? {})
const errorLogs = computed(() => summary.value?.byLevel?.error ?? 0)

const relTone = computed(() => {
  if (!rel.value.total) return 'default'
  return rel.value.resolved === rel.value.total ? 'good' : 'warn'
})

function clock(d) {
  return d ? d.toTimeString().slice(0, 8) : '—'
}
</script>

<template>
  <div class="app">
    <header class="top">
      <div>
        <h1>k8s-collector</h1>
        <p class="sub">쿠버네티스 자원의 메타데이터 · 메트릭데이터 · 로그데이터 수집 현황</p>
      </div>
      <div class="status">
        <span v-if="error" class="err">연결 실패 — {{ error }}</span>
        <span v-else class="ok">
          <i class="dot"></i> 5초마다 갱신 · 마지막 {{ clock(lastAt) }}
        </span>
      </div>
    </header>

    <p v-if="loading" class="loading">수집 서버에 연결하는 중…</p>

    <template v-else>
      <div class="cards">
        <StatCard
          label="수집 자원"
          :value="resources.length"
          :sub="`${Object.keys(summary?.byKind ?? {}).length}종`"
        />
        <StatCard
          label="관계 해석"
          :value="`${rel.resolved} / ${rel.total}`"
          :sub="rel.total === rel.resolved ? '전부 UID 해석됨' : `${rel.total - rel.resolved}건 미해석`"
          :tone="relTone"
        />
        <StatCard
          label="메트릭"
          :value="stats.metric?.items ?? 0"
          :sub="`배치 ${stats.metric?.batches ?? 0}건`"
        />
        <StatCard
          label="로그"
          :value="stats.log?.items ?? 0"
          :sub="`배치 ${stats.log?.batches ?? 0}건`"
        />
        <StatCard
          label="에러 로그"
          :value="errorLogs"
          sub="level=error"
          :tone="errorLogs > 0 ? 'bad' : 'default'"
        />
        <StatCard
          label="거절된 배치"
          :value="(stats.meta?.rejected ?? 0) + (stats.metric?.rejected ?? 0) + (stats.log?.rejected ?? 0)"
          sub="검증 실패"
          :tone="'default'"
        />
      </div>

      <div class="grid">
        <ResourceTable :resources="resources" />
        <MetricPanel :cpu="cpu" :mem="mem" />
      </div>

      <LogPanel :logs="logs" :level-count="summary?.byLevel ?? {}" />
    </template>
  </div>
</template>

<style scoped>
.app {
  max-width: 78rem;
  margin: 0 auto;
  padding: 2rem 1.5rem 4rem;
  display: flex;
  flex-direction: column;
  gap: 1.1rem;
}

.top {
  display: flex;
  flex-wrap: wrap;
  gap: 0.6rem 1rem;
  align-items: flex-end;
  justify-content: space-between;
  padding-bottom: 1rem;
  border-bottom: 1px solid var(--line);
}
h1 { font-size: 1.5rem; font-weight: 800; letter-spacing: -0.035em; }
.sub { margin: 0.2rem 0 0; font-size: 0.82rem; color: var(--muted); }

.status { font-family: var(--mono); font-size: 0.75rem; }
.ok { color: var(--muted); display: inline-flex; align-items: center; gap: 0.4rem; }
.dot {
  width: 7px; height: 7px; border-radius: 50%;
  background: var(--accent);
  animation: pulse 2s ease-in-out infinite;
}
@keyframes pulse { 0%, 100% { opacity: 1 } 50% { opacity: 0.35 } }
@media (prefers-reduced-motion: reduce) { .dot { animation: none } }
.err { color: var(--error); }

.loading { color: var(--muted); }

.cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(9.5rem, 1fr));
  gap: 0.7rem;
}

.grid {
  display: grid;
  grid-template-columns: 1.35fr 1fr;
  gap: 1.1rem;
  align-items: start;
}
@media (max-width: 62rem) {
  .grid { grid-template-columns: 1fr; }
}
</style>
