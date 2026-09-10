<script setup>
import { withBase } from "vitepress";

defineProps({
  src: { type: String, required: true },
  poster: { type: String, required: true },
  title: { type: String, required: true },
});
</script>

<template>
  <figure class="video-card">
    <video
      class="video-card-media"
      :src="withBase(src)"
      :poster="withBase(poster)"
      :aria-label="title"
      controls
      preload="none"
    ></video>
    <figcaption class="video-card-caption">
      <span class="video-card-title">{{ title }}</span>
    </figcaption>
  </figure>
</template>
