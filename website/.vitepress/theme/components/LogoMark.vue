<script setup lang="ts">
// LogoMark 是 resources/icon.svg 的内联版本，光标块像终端插入符一样缓慢闪烁。
</script>

<template>
  <svg class="logo-mark" viewBox="0 0 1024 1024" aria-hidden="true" focusable="false">
    <rect class="logo-mark-plate" width="1024" height="1024" rx="256" />
    <path fill="#fafaf9" d="M192 256h96l224 384 224-384h96L512 800z" />
    <rect class="logo-mark-cursor" x="640" y="720" width="192" height="80" fill="#60a5fa" />
  </svg>
</template>

<style scoped>
.logo-mark {
  display: block;
  width: 28px;
  height: 28px;
  border-radius: 7px;
}

.logo-mark-plate {
  fill: #1c1917;
}

/* 深色页面上墨色底板会融进背景，抬高一档并加细边。 */
:global(.dark) .logo-mark-plate {
  fill: #2f2b28;
}

:global(.dark) .logo-mark {
  box-shadow: 0 0 0 1px var(--rule-strong);
}

.logo-mark-cursor {
  animation: caret-blink 1.6s steps(1, end) infinite;
}

@media (prefers-reduced-motion: reduce) {
  .logo-mark-cursor {
    animation: none;
  }
}
</style>
