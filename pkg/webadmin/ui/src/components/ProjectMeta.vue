<template>
  <div class="project-meta">
    <a :href="projectRepositoryUrl" target="_blank" rel="noopener noreferrer">GitHub</a>
    <span aria-hidden="true">·</span>
    <span>版本 {{ appVersion }}</span>
  </div>
</template>

<script setup lang="ts">
import { appVersion, projectRepositoryUrl } from '../config/project'
</script>
