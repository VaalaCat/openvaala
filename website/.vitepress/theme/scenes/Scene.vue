<script setup lang="ts">
// Scene：按名称分派到具体场景组件；页面顶部插图、首页卖点和正文里的 <!-- scene: name --> 都经过这里。
// name 场景名；variant 决定外框：band 页面顶部插图带（装饰性，对读屏隐藏），inline 正文细线框，pillar 首页卖点框；
// caption 图注；description 正文里原有的 ASCII 示意图，作为读屏可读的文字描述保留。
import type { Component } from "vue"
import UnifiedRuntimeScene from "./pillars/UnifiedRuntimeScene.vue"
import SharedProgressScene from "./pillars/SharedProgressScene.vue"
import EcosystemScene from "./pillars/EcosystemScene.vue"
import PortableConversationsScene from "./pillars/PortableConversationsScene.vue"
import IsoQuickstartScene from "./iso/IsoQuickstartScene.vue"
import IsoInstallScene from "./iso/IsoInstallScene.vue"
import IsoTerminalScene from "./iso/IsoTerminalScene.vue"
import IsoWebScene from "./iso/IsoWebScene.vue"
import IsoDesktopScene from "./iso/IsoDesktopScene.vue"
import IsoEditorsScene from "./iso/IsoEditorsScene.vue"
import IsoCodexScene from "./iso/IsoCodexScene.vue"
import IsoConversationsScene from "./iso/IsoConversationsScene.vue"
import IsoConfigurationScene from "./iso/IsoConfigurationScene.vue"
import IsoModelsScene from "./iso/IsoModelsScene.vue"
import IsoAgentsScene from "./iso/IsoAgentsScene.vue"
import IsoExecutorsScene from "./iso/IsoExecutorsScene.vue"
import IsoToolsScene from "./iso/IsoToolsScene.vue"
import IsoExtensionsScene from "./iso/IsoExtensionsScene.vue"
import IsoTelemetryScene from "./iso/IsoTelemetryScene.vue"
import IsoCliScene from "./iso/IsoCliScene.vue"
import IsoArchitectureScene from "./iso/IsoArchitectureScene.vue"
import ConversationForkScene from "./content/ConversationForkScene.vue"
import ContextCompactionScene from "./content/ContextCompactionScene.vue"
import RuntimeLayersScene from "./content/RuntimeLayersScene.vue"
import type { SceneName } from "./scene-names"

withDefaults(defineProps<{ name: SceneName; variant?: "band" | "inline" | "pillar"; caption?: string; description?: string }>(), {
  variant: "inline",
})

const sceneComponents: Record<SceneName, Component> = {
  "unified-runtime": UnifiedRuntimeScene,
  "shared-progress": SharedProgressScene,
  "ecosystem": EcosystemScene,
  "portable-conversations": PortableConversationsScene,
  "iso-quickstart": IsoQuickstartScene,
  "iso-install": IsoInstallScene,
  "iso-terminal": IsoTerminalScene,
  "iso-web": IsoWebScene,
  "iso-desktop": IsoDesktopScene,
  "iso-editors": IsoEditorsScene,
  "iso-codex": IsoCodexScene,
  "iso-conversations": IsoConversationsScene,
  "iso-configuration": IsoConfigurationScene,
  "iso-models": IsoModelsScene,
  "iso-agents": IsoAgentsScene,
  "iso-executors": IsoExecutorsScene,
  "iso-tools": IsoToolsScene,
  "iso-extensions": IsoExtensionsScene,
  "iso-telemetry": IsoTelemetryScene,
  "iso-cli": IsoCliScene,
  "iso-architecture": IsoArchitectureScene,
  "conversation-fork": ConversationForkScene,
  "context-compaction": ContextCompactionScene,
  "runtime-layers": RuntimeLayersScene,
}
</script>

<template>
  <figure class="scene-figure" :class="`scene-figure--${variant}`" :aria-hidden="variant === 'band' ? 'true' : undefined">
    <component :is="sceneComponents[name]" />
    <pre v-if="description" class="visually-hidden">{{ description }}</pre>
    <figcaption v-if="caption">{{ caption }}</figcaption>
  </figure>
</template>
