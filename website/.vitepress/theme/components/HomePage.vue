<script setup lang="ts">
import { computed, ref } from "vue"
import { useData, withBase } from "vitepress"
import Scene from "../scenes/Scene.vue"
import type { SceneName } from "../scenes/scene-names"
import { useSiteText } from "../site"
import type { HomeFrontmatter, ThemeConfig } from "../types"

const { frontmatter, theme } = useData<ThemeConfig>()
const text = useSiteText()

const home = computed(() => frontmatter.value as HomeFrontmatter)

// 标题按逗号分成几段，每段整体换行，避免在 "cross-machine" 的连字符或中文短语中间断开。
const titleSegments = computed(() => (home.value.hero?.title ?? "OpenVaala").split(/(?<=[,，])\s*/))

// 首页四个卖点依次对应四段动画，文字来自 frontmatter.features。
const pillarScenes: SceneName[] = ["unified-runtime", "shared-progress", "ecosystem", "portable-conversations"]
const pillars = computed(() =>
  (home.value.features ?? []).slice(0, pillarScenes.length).map((feature, index) => ({
    ...feature,
    number: String(index + 1).padStart(2, "0"),
    sceneName: pillarScenes[index],
    linkTitle: feature.link ? getLinkTitle(feature.link) : undefined,
  })),
)

// getLinkTitle 用侧栏里的页面标题作为卖点链接文字；侧栏里没有的链接显示路径。
function getLinkTitle(link: string): string {
  const normalizedLink = link.replace(/\.md$/, "").replace(/\/$/, "")
  for (const group of theme.value.sidebar ?? []) {
    const item = group.items.find((sidebarItem) => sidebarItem.link === normalizedLink)
    if (item) return item.text
  }
  return normalizedLink
}

function getHref(link: string): string {
  return /^https?:\/\//.test(link) ? link : withBase(link)
}

// 首页卷首图在 docs/images/hero.png 生成前显示占位框；glob 找不到文件时返回空对象。
const heroImages = import.meta.glob<string>("/images/hero.png", { eager: true, query: "?url", import: "default" })
const heroImageUrl = Object.values(heroImages)[0]

const installCommand = "go install github.com/VaalaCat/openvaala/cmd/openvaala@latest"
const isInstallCopied = ref(false)
let installCopiedTimer: ReturnType<typeof setTimeout> | undefined

async function copyInstallCommand() {
  await navigator.clipboard.writeText(installCommand)
  isInstallCopied.value = true
  clearTimeout(installCopiedTimer)
  installCopiedTimer = setTimeout(() => {
    isInstallCopied.value = false
  }, 1800)
}
</script>

<template>
  <div class="home">
    <section class="home-hero">
      <h1 class="home-title">
        <template v-for="(segment, index) in titleSegments" :key="index">
          <span class="home-title-segment">{{ segment }}</span>{{ " " }}
        </template>
        <span class="home-title-caret" aria-hidden="true" />
      </h1>
      <p v-if="home.hero?.tagline" class="home-tagline">{{ home.hero.tagline }}</p>

      <div class="home-actions">
        <a
          v-for="(action, index) in home.hero?.actions ?? []"
          :key="action.link"
          class="home-action"
          :class="index === 0 ? 'home-action--primary' : 'home-action--secondary'"
          :href="getHref(action.link)"
        >
          {{ action.text }}
        </a>
      </div>

      <div class="home-install">
        <span class="home-install-prompt" aria-hidden="true">$</span>
        <code class="home-install-command">{{ installCommand }}</code>
        <button class="home-install-copy" type="button" :aria-label="text.copyInstall" @click="copyInstallCommand">
          {{ isInstallCopied ? text.copied : text.copy }}
        </button>
        <span class="visually-hidden" aria-live="polite">{{ isInstallCopied ? text.copied : "" }}</span>
      </div>
    </section>

    <figure class="home-shot">
      <img v-if="heroImageUrl" :src="heroImageUrl" alt="" />
      <div v-else class="home-shot-pending">
        <span>{{ text.heroImagePending }}</span>
      </div>
    </figure>

    <section v-for="(pillar, index) in pillars" :key="pillar.number" class="pillar" :class="{ 'pillar--flip': index % 2 === 1 }">
      <div class="pillar-text">
        <p class="pillar-number">{{ pillar.number }} —</p>
        <h2 class="pillar-title">{{ pillar.title }}</h2>
        <p class="pillar-details">{{ pillar.details }}</p>
        <a v-if="pillar.link" class="text-link pillar-link" :href="getHref(pillar.link)">{{ pillar.linkTitle }}</a>
      </div>
      <Scene class="pillar-figure" :name="pillar.sceneName" variant="pillar" />
    </section>

    <div class="prose home-content">
      <Content />
    </div>
  </div>
</template>

<style scoped>
.home {
  max-width: var(--home-width);
  margin: 0 auto;
  padding: 0 var(--site-gutter);
}

.home-hero {
  padding: 96px 0 48px;
}

.home-title {
  max-width: 21ch;
  margin: 0;
  color: var(--ink);
  font-family: var(--font-display);
  font-size: clamp(2.5rem, 1.5rem + 3.4vw, 4.125rem);
  font-weight: 380;
  letter-spacing: -0.025em;
  line-height: 1.04;
}

.home-title-segment {
  display: inline-block;
}

/* 标题末尾的下划线光标，与 Logo 里的光标同形同步闪烁。 */
.home-title-caret {
  display: inline-block;
  width: 0.42em;
  height: 0.1em;
  margin-left: 0.12em;
  background: var(--signal);
  animation: caret-blink 1.6s steps(1, end) infinite;
}

.home-tagline {
  max-width: 36rem;
  margin: 24px 0 0;
  color: var(--ink-soft);
  font-size: 1.25rem;
  line-height: 1.55;
}

.home-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 32px;
}

.home-action {
  display: inline-flex;
  align-items: center;
  height: 44px;
  padding: 0 20px;
  border-radius: 10px;
  font-size: 0.9375rem;
  font-weight: 500;
  text-decoration: none;
  transition: transform 160ms ease-out;
}

.home-action:active {
  transform: translateY(1px);
}

.home-action--primary {
  background: var(--ink);
  color: var(--paper);
}

.home-action--primary:hover {
  background: var(--ink-soft);
}

.home-action--secondary {
  border: 1px solid var(--rule-strong);
  color: var(--ink);
}

.home-action--secondary:hover {
  border-color: var(--ink-muted);
  background: var(--paper-raised);
}

.home-install {
  display: inline-flex;
  align-items: center;
  gap: 12px;
  max-width: 100%;
  margin-top: 20px;
  padding: 6px 6px 6px 16px;
  border: 1px solid var(--rule);
  border-radius: 12px;
  background: var(--paper-raised);
  font-family: var(--font-code);
  font-size: 0.8125rem;
}

.home-install-prompt {
  color: var(--ink-muted);
}

.home-install-command {
  overflow-x: auto;
  color: var(--ink);
  white-space: nowrap;
  scrollbar-width: none;
}

.home-install-copy {
  flex: none;
  min-width: 64px;
  height: 30px;
  padding: 0 10px;
  border: 1px solid var(--rule);
  border-radius: 8px;
  background: var(--paper);
  color: var(--ink-soft);
  font-family: var(--font-code);
  font-size: 0.6875rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  cursor: pointer;
}

.home-install-copy:hover {
  border-color: var(--rule-strong);
  color: var(--ink);
}

.home-shot {
  margin: 24px 0 0;
  overflow: hidden;
  border: 1px solid var(--rule);
  border-radius: 18px;
  background: var(--paper-raised);
}

.home-shot img {
  display: block;
  width: 100%;
  height: auto;
}

.home-shot-pending {
  display: grid;
  place-items: center;
  aspect-ratio: 16 / 9;
  background-image:
    linear-gradient(var(--rule) 1px, transparent 1px),
    linear-gradient(90deg, var(--rule) 1px, transparent 1px);
  background-position: -1px -1px;
  background-size: 48px 48px;
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.75rem;
  letter-spacing: 0.09em;
  text-transform: uppercase;
}

.home-shot-pending span {
  padding: 6px 12px;
  border: 1px solid var(--rule);
  border-radius: 8px;
  background: var(--paper-raised);
}

.pillar {
  display: grid;
  grid-template-columns: minmax(0, 5fr) minmax(0, 7fr);
  gap: 64px;
  align-items: center;
  margin-top: 120px;
  padding-top: 32px;
  border-top: 1px solid var(--rule);
}

.pillar--flip {
  grid-template-columns: minmax(0, 7fr) minmax(0, 5fr);
}

.pillar--flip .pillar-text {
  order: 2;
}

.pillar-number {
  margin: 0 0 20px;
  color: var(--ink-muted);
  font-family: var(--font-code);
  font-size: 0.75rem;
  letter-spacing: 0.08em;
}

.pillar-title {
  margin: 0;
  color: var(--ink);
  font-family: var(--font-display);
  font-size: clamp(1.75rem, 1.3rem + 1.2vw, 2.25rem);
  font-weight: 400;
  letter-spacing: -0.015em;
  line-height: 1.15;
  text-wrap: balance;
}

.pillar-details {
  margin: 20px 0 0;
  color: var(--ink-soft);
  font-size: 1.0625rem;
  line-height: 1.65;
}

.pillar-link {
  display: inline-block;
  margin-top: 20px;
  font-size: 0.9375rem;
}

.pillar-figure {
  min-width: 0;
}

.home-content {
  max-width: var(--content-width);
  padding: 96px 0 120px;
}

.home-content:not(:has(> div > *)) {
  padding-bottom: 72px;
}

@media (max-width: 959px) {
  .home-hero {
    padding-top: 72px;
  }

  .pillar,
  .pillar--flip {
    grid-template-columns: minmax(0, 1fr);
    gap: 32px;
    margin-top: 80px;
  }

  .pillar--flip .pillar-text {
    order: 0;
  }
}

@media (max-width: 719px) {
  .home-hero {
    padding: 56px 0 40px;
  }

  .home-tagline {
    font-size: 1.125rem;
  }

  .home-install {
    display: flex;
  }

  .home-shot {
    border-radius: 12px;
  }
}

@media (prefers-reduced-motion: reduce) {
  .home-title-caret {
    animation: none;
  }
}
</style>
