<script setup>
defineProps({
  label: String,
  value: [String, Number],
  sub: String,
  tone: { type: String, default: 'default' }, // default | good | warn | bad
})
</script>

<template>
  <div class="card" :class="tone">
    <p class="label">{{ label }}</p>
    <p class="value">{{ value }}</p>
    <p v-if="sub" class="sub">{{ sub }}</p>
  </div>
</template>

<style scoped>
.card {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 0.85rem 1rem;
  min-width: 0;
}
.card.good { border-left: 3px solid var(--accent); }
.card.warn { border-left: 3px solid var(--warn); }
.card.bad  { border-left: 3px solid var(--error); }

.label {
  margin: 0 0 0.35rem;
  font-size: 0.75rem;
  font-weight: 700;
  letter-spacing: 0.04em;
  color: var(--muted);
  text-transform: uppercase;
}
.value {
  margin: 0;
  font-family: var(--mono);
  font-size: 1.6rem;
  font-weight: 700;
  line-height: 1.1;
  color: var(--ink);
  font-variant-numeric: tabular-nums;
}
.sub {
  margin: 0.3rem 0 0;
  font-size: 0.75rem;
  color: var(--muted);
}
</style>
