<script setup>
import { computed, ref } from 'vue'

const props = defineProps({
  logs: { type: Array, default: () => [] },
  levelCount: { type: Object, default: () => ({}) },
})

const level = ref('')
const namespace = ref('')

const namespaces = computed(() =>
  [...new Set(props.logs.map((l) => l.namespace))].sort()
)

const rows = computed(() =>
  props.logs.filter((l) => {
    if (level.value && l.level !== level.value) return false
    if (namespace.value && l.namespace !== namespace.value) return false
    return true
  })
)

function time(iso) {
  const d = new Date(iso)
  return d.toTimeString().slice(0, 8)
}
</script>

<template>
  <section class="panel">
    <header>
      <h2>수집된 로그</h2>
      <div class="filters">
        <button
          v-for="lv in ['error', 'warn', 'info', 'unknown']"
          :key="lv"
          class="chip"
          :class="[lv, { on: level === lv }]"
          @click="level = level === lv ? '' : lv"
        >
          {{ lv }} <b>{{ levelCount[lv] ?? 0 }}</b>
        </button>
        <select v-model="namespace">
          <option value="">전체 네임스페이스</option>
          <option v-for="n in namespaces" :key="n" :value="n">{{ n }}</option>
        </select>
      </div>
    </header>

    <div class="scroll">
      <div v-for="(l, i) in rows" :key="i" class="line">
        <span class="t">{{ time(l.at) }}</span>
        <span class="lv" :class="l.level">{{ l.level }}</span>
        <span class="src">{{ l.namespace }}/{{ l.podName }}</span>
        <span class="msg">{{ l.message }}</span>
      </div>
      <p v-if="!rows.length" class="empty">해당 조건의 로그가 없습니다.</p>
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
  flex-wrap: wrap;
  gap: 0.6rem 1rem;
  align-items: center;
  justify-content: space-between;
  padding: 0.75rem 1rem;
  border-bottom: 1px solid var(--line);
}
h2 { font-size: 0.95rem; font-weight: 750; }
.filters { display: flex; flex-wrap: wrap; gap: 0.35rem; align-items: center; }

.chip {
  background: var(--panel-2);
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 0.12rem 0.6rem;
  font-family: var(--mono);
  font-size: 0.72rem;
  color: var(--muted);
  cursor: pointer;
}
.chip b { font-variant-numeric: tabular-nums; }
.chip.on { border-color: currentColor; }
.chip.error.on { color: var(--error); }
.chip.warn.on { color: var(--warn); }
.chip.info.on { color: var(--info); }
.chip.unknown.on { color: var(--body); }

select {
  background: var(--panel-2);
  color: var(--body);
  border: 1px solid var(--line);
  border-radius: 4px;
  padding: 0.22rem 0.5rem;
  font-family: inherit;
  font-size: 0.78rem;
}

.scroll { max-height: 22rem; overflow: auto; padding: 0.4rem 0; }
.line {
  display: grid;
  grid-template-columns: 4.5rem 3.6rem minmax(0, 14rem) 1fr;
  gap: 0.7rem;
  padding: 0.18rem 1rem;
  font-family: var(--mono);
  font-size: 0.75rem;
  align-items: baseline;
}
.line:hover { background: var(--panel-2); }
.t { color: var(--muted); }
.lv { font-weight: 700; }
.lv.error { color: var(--error); }
.lv.warn { color: var(--warn); }
.lv.info { color: var(--info); }
.lv.unknown { color: var(--muted); }
.src { color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.msg { color: var(--body); word-break: break-all; }
.empty { color: var(--muted); padding: 1rem; margin: 0; font-size: 0.8rem; }

@media (max-width: 46rem) {
  .line { grid-template-columns: 4.5rem 3.6rem 1fr; }
  .src { display: none; }
}
</style>
