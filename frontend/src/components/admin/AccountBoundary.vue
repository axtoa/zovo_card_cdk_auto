<template>
  <section class="card mb-4">
    <div class="flex flex-wrap items-center gap-3">
      <strong>{{ issuing ? '发码 / 库存账号' : '查看账号' }}</strong>
      <el-select v-model="selected" placeholder="请选择卡台账号" style="width: 330px; max-width: 100%" :disabled="loading || busy > 0">
        <el-option v-for="a in accounts" :key="a.id" :value="a.id" :label="`${a.name} · 可用 ${balances[a.id]?.value ?? '—'} USD${balances[a.id]?.error ? '（刷新失败）' : ''}`" />
      </el-select>
      <el-button :loading="refreshing" @click="refreshBalances">刷新余额</el-button>
      <router-link to="/ops/integration" class="text-sm text-muted">管理账号</router-link>
    </div>
    <p class="text-sm text-muted mt-2">{{ issuing ? '新码由所选账号生成并承担后续费用；其他账号的旧码继续正常兑换。' : '只影响本页展示和操作。所有账号同时可用，业务规则全站共用。' }}</p>
    <p v-if="selected && balances[selected]?.time" class="text-xs text-muted mt-1">余额更新：{{ balances[selected].time }}<span v-if="balances[selected].error"> · {{ balances[selected].error }}（上次余额仅供参考）</span></p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="mt-2" />
    <el-button v-if="error" @click="load">重试</el-button>
  </section>
  <slot v-if="selected" :key="selected" :account-id="selected" />
  <el-empty v-else :description="loading ? '正在读取账号…' : '请选择账号后继续'" />
</template>
<script setup lang="ts">
import { onMounted, onUnmounted, provide, reactive, ref } from 'vue'
import { authFetch } from '../../lib/api'
import { accountKey, accountBusyKey, type CardAccount } from '../../lib/cardAccounts'
defineProps<{ issuing?: boolean }>()
const accounts = ref<CardAccount[]>([])
const selected = ref<number>(0)
const loading = ref(false)
const refreshing = ref(false)
const error = ref('')
const balances = reactive<Record<number, { value?: string; time?: string; error?: string }>>({})
const controller = new AbortController()
onUnmounted(() => controller.abort())
const busy = ref(0)
provide(accountKey, selected)
provide(accountBusyKey, busy)
async function refreshBalances() {
  if (refreshing.value) return
  refreshing.value = true
  try {
    // Limit pressure on the upstream; each failed account remains independent.
    for (let i = 0; i < accounts.value.length; i += 3) {
      if (controller.signal.aborted) break
      await Promise.allSettled(accounts.value.slice(i, i + 3).map(async a => {
        try {
          const r = await authFetch('/api/v1/admin/cardplatform/balance', { headers: { 'X-Card-Account-ID': String(a.id) }, signal: controller.signal })
          const d = await r.json()
          if (!r.ok) throw new Error(d.error || '余额刷新失败')
          balances[a.id] = { value: d.spendable_balance == null ? undefined : String(d.spendable_balance), time: new Date().toLocaleString() }
        } catch (e: any) { balances[a.id] = { ...balances[a.id], error: e.message } }
      }))
    }
  } finally { refreshing.value = false }
}
async function load() {
  loading.value = true; error.value = ''
  try {
    const r = await authFetch('/api/v1/admin/cardplatform/accounts', { signal: controller.signal })
    const d = await r.json()
    if (!r.ok) throw new Error(d.error || '读取账号失败')
    accounts.value = d.accounts || []
    if (accounts.value.length === 1) selected.value = accounts.value[0].id
    void refreshBalances()
  } catch (e: any) { error.value = e.message }
  finally { loading.value = false }
}
onMounted(load)
</script>
