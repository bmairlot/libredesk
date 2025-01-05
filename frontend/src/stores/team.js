import { ref, computed } from 'vue'
import { defineStore } from 'pinia'
import { handleHTTPError } from '@/utils/http'
import { useEmitter } from '@/composables/useEmitter'
import { EMITTER_EVENTS } from '@/constants/emitterEvents'
import api from '@/api'

export const useTeamStore = defineStore('team', () => {
    const teams = ref([])
    const emitter = useEmitter()
    const forSelect = computed(() => teams.value.map(team => ({
        label: team.name,
        value: team.id
    })))
    const fetchTeams = async () => {
        if (teams.value.length) return
        try {
            const response = await api.getTeamsCompact()
            teams.value = response?.data?.data || []
        } catch (error) {
            emitter.emit(EMITTER_EVENTS.SHOW_TOAST, {
                title: 'Error',
                variant: 'destructive',
                description: handleHTTPError(error).message
            })
        }
    }
    return {
        teams,
        forSelect,
        fetchTeams,
    }
})