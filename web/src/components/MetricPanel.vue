<script setup>
import { computed, ref } from 'vue'

const props = defineProps({
  cpu: { type: Array, default: () => [] },
  mem: { type: Array, default: () => [] },
})

const view = ref('cpu')

const rows = computed(() => (view.value === 'cpu' ? props.cpu : props.mem))
const max = computed(() => Math.max(1, ...rows.value.map((r) => r.value)))

function label(v) {
  if (view.value === 'cpu') return `${Math.round(v)}m`
  const mb = v / (1024 * 1024)
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)}Gi` : `${Math.round(mb)}Mi`
}
</script>

<template>
  <section class="panel">
    <header>
      <h2>파드 리소스 사용량</h2>
      <div class="toggle">
        <button :class="{ on: view === 'cpu' }" @click="view = 'cpu'">CPU</button>
        <button :class="{ on: view === 'mem' }" @click="view = 'mem'">메모리</button>
      </div>
    </header>

    <div class="bars">
      <div v-for="r in rows" :key="r.resourceUid + r.name" class="bar-row">
        <span class="pod" :title="r.resourceName">{{ r.resourceName }}</span>
        <span class="ns">{{ r.namespace }}</span>
        <div class="track">
          <div class="fill" :style="{ width: `${(r.value / max) * 100}%` }"></div>
        </div>
        <span class="val">{{ label(r.value) }}</span>
      </div>
      <p v-if="!rows.length" class="empty">
        메트릭이 아직 없습니다. metrics-server 가 준비되면 채워집니다.
      </p>
    </div>
  </section>
</template>

<style scoped>
.panel {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 6px;
  overflow: hidden;
}
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.75rem 1rem;
  border-bottom: 1px solid var(--line);
}
h2 { font-size: 0.95rem; font-weight: 750; }

.toggle { display: flex; border: 1px solid var(--line); border-radius: 4px; overflow: hidden; }
.toggle button {
  background: var(--panel-2);
  color: var(--muted);
  border: 0;
  padding: 0.22rem 0.7rem;
  font-family: inherit;
  font-size: 0.78rem;
  cursor: pointer;
}
.toggle button.on { background: var(--accent-dim); color: var(--accent); font-weight: 700; }

.bars { padding: 0.7rem 1rem 1rem; display: flex; flex-direction: column; gap: 0.45rem; }
.bar-row {
  display: grid;
  grid-template-columns: minmax(0, 12rem) 4.5rem 1fr 4.5rem;
  gap: 0.6rem;
  align-items: center;
  font-size: 0.78rem;
}
.pod {
  font-family: var(--mono);
  color: var(--ink);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ns { font-size: 0.72rem; color: var(--muted); }
.track { height: 7px; background: var(--panel-2); border-radius: 4px; overflow: hidden; }
.fill { height: 100%; background: var(--accent); border-radius: 4px; }
.val {
  font-family: var(--mono);
  text-align: right;
  color: var(--body);
  font-variant-numeric: tabular-nums;
}
.empty { color: var(--muted); margin: 0.5rem 0; font-size: 0.8rem; }

@media (max-width: 40rem) {
  .bar-row { grid-template-columns: 1fr 3.5rem; grid-template-areas: 'pod val' 'track track'; }
  .ns { display: none; }
  .pod { grid-area: pod; }
  .val { grid-area: val; }
  .track { grid-area: track; }
}
</style>
