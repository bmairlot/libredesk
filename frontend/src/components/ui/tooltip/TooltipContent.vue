<script setup>
import { computed } from 'vue'
import { TooltipContent, TooltipPortal, useForwardPropsEmits } from 'radix-vue'
import { cn } from '@/lib/utils'

defineOptions({
  inheritAttrs: false
})

const props = defineProps({
  ariaLabel: { type: String, required: false },
  asChild: { type: Boolean, required: false },
  as: { type: null, required: false },
  side: { type: null, required: false },
  sideOffset: { type: Number, required: false, default: 4 },
  align: { type: null, required: false },
  alignOffset: { type: Number, required: false },
  avoidCollisions: { type: Boolean, required: false },
  collisionBoundary: { type: null, required: false },
  collisionPadding: { type: [Number, Object], required: false },
  arrowPadding: { type: Number, required: false },
  sticky: { type: String, required: false },
  hideWhenDetached: { type: Boolean, required: false },
  class: { type: null, required: false }
})

const emits = defineEmits(['escapeKeyDown', 'pointerDownOutside'])

const delegatedProps = computed(() => {
  const { class: _, ...delegated } = props

  return delegated
})

const forwarded = useForwardPropsEmits(delegatedProps, emits)
</script>

<template>
  <TooltipPortal>
    <TooltipContent
      v-bind="{ ...forwarded, ...$attrs }"
      :class="
        cn(
          'z-50 overflow-hidden rounded-md bg-primary px-3 py-1.5 text-xs text-primary-foreground animate-in fade-in-0 zoom-in-95 data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=closed]:zoom-out-95 data-[side=bottom]:slide-in-from-top-2 data-[side=left]:slide-in-from-right-2 data-[side=right]:slide-in-from-left-2 data-[side=top]:slide-in-from-bottom-2',
          props.class
        )
      "
    >
      <slot />
    </TooltipContent>
  </TooltipPortal>
</template>
