<template>
  <div class="plugin-management">
    <header class="section-header plugin-header">
      <div>
        <h2>{{ t('pluginManagement.title') }}</h2>
        <p class="section-description">{{ t('pluginManagement.description') }}</p>
      </div>
      <t-button variant="outline" :loading="loading" @click="loadPlugins">
        <template #icon><t-icon name="refresh" /></template>
        {{ t('pluginManagement.refresh') }}
      </t-button>
    </header>

    <div v-if="loading && !loadedOnce" class="plugin-state">
      <t-loading size="small" />
      <span>{{ t('pluginManagement.loading') }}</span>
    </div>
    <div v-else-if="error" class="plugin-state plugin-state--error" role="alert">
      <t-icon name="error-circle" size="22px" />
      <span>{{ error }}</span>
      <t-button size="small" variant="outline" @click="loadPlugins">
        {{ t('pluginManagement.retry') }}
      </t-button>
    </div>
    <t-empty v-else-if="plugins.length === 0" :description="t('pluginManagement.empty')" />
    <div v-else class="plugin-table-shell">
      <t-table row-key="id" :data="plugins" :columns="columns" hover>
        <template #name="{ row }">
          <div class="plugin-name-cell">
            <strong>{{ row.name }}</strong>
            <span>{{ row.id }}</span>
          </div>
        </template>
        <template #extension_type="{ row }">
          <t-tag theme="primary" variant="light">{{ row.extension_type }}</t-tag>
        </template>
        <template #status="{ row }">
          <t-tag :theme="statusTheme(row.status)" variant="light">{{ statusLabel(row.status) }}</t-tag>
        </template>
        <template #last_error="{ row }">
          <span v-if="row.last_error" class="plugin-error" :title="row.last_error">{{ row.last_error }}</span>
          <span v-else>—</span>
        </template>
        <template #actions="{ row }">
          <div class="plugin-actions">
            <t-button size="small" variant="text" @click="openAudit(row)">
              {{ t('pluginManagement.audit') }}
            </t-button>
            <t-popconfirm
              v-if="row.status === 'failed' && row.restart_policy?.enabled"
              :content="t('pluginManagement.restartConfirm', { name: row.name })"
              @confirm="restartPlugin(row)"
            >
              <t-button size="small" theme="warning" variant="text" :loading="restartingID === row.id">
                {{ t('pluginManagement.restart') }}
              </t-button>
            </t-popconfirm>
          </div>
        </template>
      </t-table>
    </div>

    <SettingDrawer
      v-model:visible="auditVisible"
      :title="t('pluginManagement.auditTitle', { name: selectedPlugin?.name || '' })"
      :description="t('pluginManagement.auditDescription')"
      icon="history"
      width="680px"
      :min-width="480"
      :max-width="960"
      storage-key="setting-drawer:width:plugin-audit"
      hide-footer
    >
      <div class="plugin-audit-toolbar">
        <t-select
          v-model="auditAction"
          :aria-label="t('pluginManagement.auditFilter')"
          :options="auditActionOptions"
          clearable
          :placeholder="t('pluginManagement.auditAllActions')"
          @change="loadAudit"
        />
        <t-button variant="outline" size="small" :loading="auditLoading" @click="loadAudit">
          <template #icon><t-icon name="refresh" /></template>
          {{ t('pluginManagement.auditRefresh') }}
        </t-button>
      </div>
      <div v-if="auditLoading" class="plugin-state"><t-loading size="small" /></div>
      <div v-else-if="auditError" class="plugin-state plugin-state--error" role="alert">
        <span>{{ auditError }}</span>
        <t-button size="small" variant="outline" @click="loadAudit">{{ t('pluginManagement.retry') }}</t-button>
      </div>
      <t-empty v-else-if="auditEvents.length === 0" :description="t('pluginManagement.auditEmpty')" />
      <div v-else class="plugin-audit-list">
        <article v-for="event in auditEvents" :key="event.id" class="plugin-audit-event">
          <div class="plugin-audit-event__topline">
            <strong>{{ event.action }}</strong>
            <t-tag :theme="event.outcome === 'success' ? 'success' : 'warning'" variant="light">{{ event.outcome }}</t-tag>
          </div>
          <time :datetime="event.timestamp">{{ formatDate(event.timestamp) }}</time>
          <dl v-if="event.details && Object.keys(event.details).length" class="plugin-audit-details">
            <template v-for="(value, key) in event.details" :key="key">
              <dt>{{ key }}</dt><dd>{{ value }}</dd>
            </template>
          </dl>
        </article>
      </div>
    </SettingDrawer>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { MessagePlugin } from 'tdesign-vue-next'
import SettingDrawer from '@/components/settings/SettingDrawer.vue'
import {
  listPlugins,
  listPluginAudit,
  restartPlugin as requestPluginRestart,
  type Plugin,
  type PluginAuditEvent,
} from '@/api/system'

const { t, locale } = useI18n()
const plugins = ref<Plugin[]>([])
const loading = ref(false)
const loadedOnce = ref(false)
const error = ref('')
const restartingID = ref('')
const auditVisible = ref(false)
const selectedPlugin = ref<Plugin | null>(null)
const auditEvents = ref<PluginAuditEvent[]>([])
const auditLoading = ref(false)
const auditError = ref('')
const auditAction = ref<string | undefined>()

const auditActionOptions = computed(() => [
  'plugin.started',
  'plugin.start_failed',
  'plugin.stopped',
  'plugin.stop_failed',
  'plugin.health_failed',
  'plugin.identity_failed',
  'plugin.network_denied',
  'plugin.runtime_failed',
  'plugin.restarted',
  'plugin.restart_denied',
].map(value => ({ label: value, value })))

const columns = computed(() => [
  { colKey: 'name', title: t('pluginManagement.name'), minWidth: 180 },
  { colKey: 'version', title: t('pluginManagement.version'), width: 104 },
  { colKey: 'extension_type', title: t('pluginManagement.type'), width: 118 },
  { colKey: 'status', title: t('pluginManagement.status'), width: 112 },
  { colKey: 'last_error', title: t('pluginManagement.lastError'), minWidth: 180 },
  { colKey: 'actions', title: t('pluginManagement.actions'), width: 142, align: 'right' as const },
])

async function loadPlugins() {
  loading.value = true
  error.value = ''
  try {
    plugins.value = (await listPlugins()).data || []
    loadedOnce.value = true
  } catch (err: any) {
    error.value = err?.message || t('pluginManagement.loadFailed')
  } finally {
    loading.value = false
  }
}

async function restartPlugin(plugin: Plugin) {
  restartingID.value = plugin.id
  try {
    await requestPluginRestart(plugin.id)
    MessagePlugin.success(t('pluginManagement.restartSuccess', { name: plugin.name }))
    await loadPlugins()
  } catch (err: any) {
    MessagePlugin.error(err?.message || t('pluginManagement.restartFailed'))
  } finally {
    restartingID.value = ''
  }
}

async function openAudit(plugin: Plugin) {
  selectedPlugin.value = plugin
  auditAction.value = undefined
  auditVisible.value = true
  await loadAudit()
}

async function loadAudit() {
  if (!selectedPlugin.value) return
  auditLoading.value = true
  auditError.value = ''
  try {
    auditEvents.value = (await listPluginAudit(selectedPlugin.value.id, {
      action: auditAction.value,
      limit: 100,
    })).data || []
  } catch (err: any) {
    auditError.value = err?.message || t('pluginManagement.auditLoadFailed')
  } finally {
    auditLoading.value = false
  }
}

function statusTheme(status: Plugin['status']) {
  if (status === 'running') return 'success'
  if (status === 'failed') return 'danger'
  if (status === 'disabled') return 'warning'
  return 'default'
}

function statusLabel(status: Plugin['status']) {
  return t(`pluginManagement.statuses.${status}`)
}

function formatDate(value: string) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(locale.value, { hour12: false })
}

onMounted(() => {
  void loadPlugins()
})
</script>

<style lang="less" scoped>
.plugin-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 20px; margin-bottom: 24px; }
.section-header h2 { margin: 0 0 8px; font-size: 20px; }
.section-description { margin: 0; color: var(--td-text-color-secondary); line-height: 1.6; }
.plugin-state { display: flex; align-items: center; justify-content: center; gap: 12px; min-height: 150px; color: var(--td-text-color-secondary); }
.plugin-state--error { flex-direction: column; border: 1px solid var(--td-error-color-3); border-radius: 8px; color: var(--td-error-color); }
.plugin-table-shell { overflow-x: auto; border: 1px solid var(--td-component-stroke); border-radius: 10px; }
.plugin-name-cell { display: flex; flex-direction: column; gap: 3px; }
.plugin-name-cell span { color: var(--td-text-color-placeholder); font-family: ui-monospace, monospace; font-size: 12px; }
.plugin-error { display: block; overflow: hidden; max-width: 280px; color: var(--td-error-color); text-overflow: ellipsis; white-space: nowrap; }
.plugin-actions { display: flex; justify-content: flex-end; gap: 4px; }
.plugin-audit-toolbar { display: flex; justify-content: space-between; gap: 12px; margin-bottom: 16px; }
.plugin-audit-toolbar :deep(.t-select) { flex: 1; }
.plugin-audit-list { overflow: hidden; border: 1px solid var(--td-component-stroke); border-radius: 8px; }
.plugin-audit-event { padding: 14px 16px; border-bottom: 1px solid var(--td-component-stroke); }
.plugin-audit-event:last-child { border-bottom: 0; }
.plugin-audit-event__topline { display: flex; justify-content: space-between; gap: 10px; }
.plugin-audit-event time { display: block; margin-top: 5px; color: var(--td-text-color-placeholder); font-size: 12px; }
.plugin-audit-details { display: grid; grid-template-columns: 120px minmax(0, 1fr); gap: 5px 10px; margin: 10px 0 0; font-size: 12px; }
.plugin-audit-details dt { color: var(--td-text-color-placeholder); }
.plugin-audit-details dd { margin: 0; overflow-wrap: anywhere; }
@media (max-width: 640px) {
  .plugin-header, .plugin-audit-toolbar { flex-direction: column; }
  .plugin-audit-toolbar :deep(.t-select) { width: 100%; }
}
</style>
