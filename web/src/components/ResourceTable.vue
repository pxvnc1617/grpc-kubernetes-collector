<script setup>
import { computed, ref } from 'vue'

const props = defineProps({
  resources: { type: Array, default: () => [] },
})

const kindFilter = ref('')
const nsFilter = ref('')
const expanded = ref(null)

const kinds = computed(() =>
  [...new Set(props.resources.map((r) => r.kind))].sort()
)
const namespaces = computed(() =>
  [...new Set(props.resources.map((r) => r.namespace || '(cluster)'))].sort()
)

const rows = computed(() =>
  props.resources.filter((r) => {
    if (kindFilter.value && r.kind !== kindFilter.value) return false
    if (nsFilter.value && (r.namespace || '(cluster)') !== nsFilter.value) return false
    return true
  })
)

function toggle(uid) {
  expanded.value = expanded.value === uid ? null : uid
}

function statusTone(status) {
  if (['Running', 'Available', 'Active', 'Complete'].includes(status)) return 'good'
  if (['Progressing', 'Pending', 'Scheduled'].includes(status)) return 'warn'
  if (['Failed'].includes(status)) return 'bad'
  return ''
}
</script>

<template>
  <section class="panel">
    <header>
      <h2>수집된 자원</h2>
      <div class="filters">
        <select v-model="kindFilter">
          <option value="">종류 전체</option>
          <option v-for="k in kinds" :key="k" :value="k">{{ k }}</option>
        </select>
        <select v-model="nsFilter">
          <option value="">네임스페이스 전체</option>
          <option v-for="n in namespaces" :key="n" :value="n">{{ n }}</option>
        </select>
        <span class="count">{{ rows.length }} / {{ resources.length }}</span>
      </div>
    </header>

    <div class="scroll">
      <table>
        <thead>
          <tr>
            <th style="width: 7.5rem">종류</th>
            <th>이름</th>
            <th style="width: 7rem">네임스페이스</th>
            <th style="width: 6.5rem">상태</th>
            <th style="width: 5rem">관계</th>
          </tr>
        </thead>
        <tbody>
          <template v-for="r in rows" :key="r.uid">
            <tr class="row" :class="{ open: expanded === r.uid }" @click="toggle(r.uid)">
              <td><span class="kind">{{ r.kind }}</span></td>
              <td class="name">{{ r.name }}</td>
              <td class="ns">{{ r.namespace || '—' }}</td>
              <td>
                <span class="status" :class="statusTone(r.status)">{{ r.status }}</span>
              </td>
              <td class="rel-count">{{ (r.relations || []).length }}</td>
            </tr>
            <tr v-if="expanded === r.uid" class="detail">
              <td colspan="5">
                <div class="detail-grid">
                  <div v-if="Object.keys(r.attributes || {}).length">
                    <h3>속성</h3>
                    <dl>
                      <template v-for="(v, k) in r.attributes" :key="k">
                        <dt>{{ k }}</dt><dd>{{ v }}</dd>
                      </template>
                    </dl>
                  </div>
                  <div v-if="Object.keys(r.labels || {}).length">
                    <h3>라벨</h3>
                    <dl>
                      <template v-for="(v, k) in r.labels" :key="k">
                        <dt>{{ k }}</dt><dd>{{ v }}</dd>
                      </template>
                    </dl>
                  </div>
                  <div v-if="(r.relations || []).length" class="wide">
                    <h3>관계</h3>
                    <ul class="rels">
                      <li v-for="(rel, i) in r.relations" :key="i">
                        <span class="rtype" :class="rel.type === 'PARENT_CHILD' ? 'own' : 'dep'">
                          {{ rel.type === 'PARENT_CHILD' ? '소유' : '참조' }}
                        </span>
                        <span class="rtarget">{{ rel.targetKind }}/{{ rel.targetName }}</span>
                        <span class="rmark" :class="rel.resolved ? 'ok' : 'no'">
                          {{ rel.resolved ? 'UID 해석됨' : '미해석' }}
                        </span>
                      </li>
                    </ul>
                  </div>
                </div>
              </td>
            </tr>
          </template>
          <tr v-if="!rows.length">
            <td colspan="5" class="empty">수집된 자원이 없습니다.</td>
          </tr>
        </tbody>
      </table>
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
.filters { display: flex; gap: 0.5rem; align-items: center; }
select {
  background: var(--panel-2);
  color: var(--body);
  border: 1px solid var(--line);
  border-radius: 4px;
  padding: 0.25rem 0.5rem;
  font-family: inherit;
  font-size: 0.8125rem;
}
.count { font-family: var(--mono); font-size: 0.75rem; color: var(--muted); }

.scroll { max-height: 26rem; overflow: auto; }
table { width: 100%; border-collapse: collapse; font-size: 0.8125rem; }
thead th {
  position: sticky;
  top: 0;
  background: var(--panel-2);
  text-align: left;
  font-weight: 700;
  font-size: 0.72rem;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--muted);
  padding: 0.45rem 0.8rem;
  border-bottom: 1px solid var(--line);
}
tbody td { padding: 0.4rem 0.8rem; border-bottom: 1px solid var(--line-soft); }
.row { cursor: pointer; }
.row:hover { background: var(--panel-2); }
.row.open { background: var(--panel-2); }

.kind {
  font-family: var(--mono);
  font-size: 0.72rem;
  padding: 0.1rem 0.4rem;
  border: 1px solid var(--line);
  border-radius: 3px;
  color: var(--body);
}
.name { color: var(--ink); font-weight: 500; }
.ns { font-family: var(--mono); font-size: 0.75rem; color: var(--muted); }
.rel-count { font-family: var(--mono); color: var(--muted); }

.status { font-size: 0.75rem; }
.status.good { color: var(--accent); }
.status.warn { color: var(--warn); }
.status.bad { color: var(--error); }

.detail { background: #12171d; }
.detail-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
  gap: 1rem;
  padding: 0.4rem 0.4rem 0.8rem;
}
.detail-grid .wide { grid-column: 1 / -1; }
.detail h3 {
  font-size: 0.72rem;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--muted);
  margin-bottom: 0.35rem;
}
dl { display: grid; grid-template-columns: auto 1fr; gap: 0.15rem 0.7rem; margin: 0; font-size: 0.78rem; }
dt { font-family: var(--mono); color: var(--muted); }
dd { margin: 0; color: var(--body); word-break: break-all; }

.rels { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 0.25rem; }
.rels li { display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; font-size: 0.78rem; }
.rtype {
  font-size: 0.68rem;
  padding: 0.05rem 0.35rem;
  border-radius: 3px;
  font-weight: 700;
}
.rtype.own { background: var(--accent-dim); color: var(--accent); }
.rtype.dep { background: #2a2233; color: #b490d8; }
.rtarget { font-family: var(--mono); color: var(--ink); }
.rmark { font-size: 0.7rem; }
.rmark.ok { color: var(--accent); }
.rmark.no { color: var(--error); }

.empty { text-align: center; color: var(--muted); padding: 1.5rem; }
</style>
