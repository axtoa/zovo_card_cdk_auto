<template>
  <div class="space-y-4">
    <el-card shadow="never">
      <template #header>
        <div class="flex items-center justify-between">
          <span class="font-semibold">卡台 Webhook 回调</span>
          <el-tag size="small" type="success">验签 + 幂等入库</el-tag>
        </div>
      </template>
      <p class="text-sm text-muted mb-3">
        兑换结果仍可用轮询 <code>/public/cdk/result</code>；配置 Webhook 后卡台会主动推送
        <code>gpt_direct.completed</code> / 卡交易等事件（OpenAPI §7）。
      </p>
      <el-form label-width="140px" class="max-w-2xl">
        <el-form-item label="回调 URL">
          <el-input :model-value="webhookUrl" readonly>
            <template #append>
              <el-button @click="copy(webhookUrl)">复制</el-button>
            </template>
          </el-input>
          <div class="text-xs text-subtle mt-1">填到卡台「开发者」页的回调地址（须 https）</div>
        </el-form-item>
        <el-form-item label="各账号密钥"><router-link to="/ops/integration" class="btn-secondary">前往卡台接入管理</router-link></el-form-item>
        <el-form-item><el-button :loading="loading" @click="load">刷新事件</el-button></el-form-item>
      </el-form>
      <p class="text-sm text-muted">所有账号共用此地址，使用各自的密钥验签。下方标明每条事件所属账号。</p>
    </el-card>

    <el-card shadow="never" header="最近事件">
      <div v-if="error" class="alert alert-error mb-3">{{ error }}</div>
      <table class="data-table">
        <thead>
          <tr>
            <th>ID</th>
            <th>账号</th><th>类型</th>
            <th>幂等键</th>
            <th>时间</th>
            <th>摘要</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="loading">
            <td colspan="6" class="py-6 text-center text-muted">加载中…</td>
          </tr>
          <tr v-else-if="!events.length">
            <td colspan="6" class="py-6 text-center text-muted">暂无回调（先配 URL + secret，或继续用轮询）</td>
          </tr>
          <tr v-for="e in events" :key="e.id">
            <td class="mono">{{ e.id }}</td>
            <td>{{ accounts.find(a => a.id === e.account_id)?.name || `#${e.account_id}` }}</td><td>{{ e.event_type }}</td>
            <td class="mono text-xs">{{ e.idem_key }}</td>
            <td class="text-sm text-muted">{{ e.created_at }}</td>
            <td class="text-xs text-subtle">{{ summarize(e.payload) }}</td>
          </tr>
        </tbody>
      </table>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { authFetch } from '../../lib/api'
import { dialog } from '../../lib/dialog'

const webhookUrl = ref(`${location.origin}/api/v1/webhooks/cardplatform`)
const accounts = ref<any[]>([])
const events = ref<any[]>([])
const loading = ref(false)
const error = ref('')

function summarize(p: any) {
  if (!p || typeof p !== 'object') return '—'
  if (p.type === 'gpt_direct.completed' || p.order_id) {
    return `order=${p.order_id || ''} plan=${p.plan || ''} status=${p.status || ''}`
  }
  if (p.event === 'card_transaction') {
    return `${p.type || ''} ${p.status || ''} ${p.merchant_name || p.merchant || ''}`
  }
  if (p.event === 'card_operation') {
    return `${p.operation || ''} ${p.status || ''}`
  }
  return Object.keys(p).slice(0, 4).join(',')
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const r = await authFetch('/api/v1/admin/webhooks/events')
    const d = await r.json().catch(() => ({}))
    if (!r.ok) {
      error.value = d.error || '加载失败'
      return
    }
    if (d.webhook_url) webhookUrl.value = d.webhook_url
    events.value = d.events || []
  } finally {
    loading.value = false
  }
}

async function copy(t: string) {
  try {
    await navigator.clipboard.writeText(t)
    dialog.toast('已复制', 'ok')
  } catch {
    dialog.toast('复制失败', 'err')
  }
}

onMounted(async () => {
 const r = await authFetch('/api/v1/admin/cardplatform/accounts'); if (r.ok) accounts.value = (await r.json()).accounts || []
 await load()
})
</script>
