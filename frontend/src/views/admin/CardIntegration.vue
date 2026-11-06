<template>
  <div class="space-y-4">
    <section class="card">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div><h2 class="text-xl font-bold">卡台账号</h2><p class="text-sm text-muted mt-1">所有账号同时可用。发码时选择账号，卡密后续兑换、查单和补发始终归属原账号。</p></div>
        <div class="flex gap-2"><el-button :loading="refreshing" @click="refreshAll">刷新全部余额</el-button><el-button type="primary" @click="edit()">新增账号</el-button></div>
      </div>
      <el-alert v-if="error" :title="error" type="error" :closable="false" class="mt-3" />
      <div class="overflow-x-auto mt-4">
        <table class="data-table" style="min-width: 880px">
          <thead><tr><th>账号</th><th>可消费余额（USD）</th><th>总余额 / 保证金</th><th>余额更新时间</th><th>回调密钥</th><th>操作</th></tr></thead>
          <tbody><tr v-for="a in accounts" :key="a.id">
            <td><strong>{{ a.name }}</strong><div class="text-xs text-muted">#{{ a.id }} · {{ a.api_key_hint || '待配置 Key' }}</div></td>
            <td><strong class="text-lg">{{ balances[a.id]?.spendable ?? '—' }}</strong><div v-if="balances[a.id]?.error" class="text-xs text-red-500">{{ balances[a.id].error }}</div></td>
            <td>{{ balances[a.id]?.total ?? '—' }} / {{ balances[a.id]?.reserve ?? '—' }}</td>
            <td class="text-xs text-muted">{{ balances[a.id]?.time ?? '尚未查询' }}<div v-if="balances[a.id]?.error && balances[a.id]?.time">上次成功数据，仅供参考</div></td>
            <td><el-tag :type="a.webhook_secret_configured ? 'success' : 'warning'">{{ a.webhook_secret_configured ? '已配置' : '待配置' }}</el-tag></td>
            <td><el-button size="small" :loading="balances[a.id]?.loading" @click="refreshBalance(a)">刷新余额</el-button><el-button size="small" @click="edit(a)">编辑</el-button></td>
          </tr></tbody>
        </table>
      </div>
      <p class="text-xs text-muted mt-3">余额来自各账号的卡台接口；查询失败显示错误并保留上次成功时间，不会把失败当作余额为零。</p>
      <router-link to="/ops/cdkeys" class="btn-primary inline-block mt-3">去发码</router-link>
    </section>
    <section class="card space-y-3">
      <h3 class="font-semibold">共用接入信息</h3>
      <el-form label-position="top" class="max-w-2xl">
        <el-form-item label="卡台站点"><el-input v-model="base" :disabled="accounts.some(a => a.api_key_configured)" /></el-form-item>
        <p class="text-xs text-muted mb-3">账号使用同一卡台。配置账号后固定站点，避免旧卡密被发送到其他平台。</p>
        <el-form-item label="共用回调地址"><el-input :model-value="webhookUrl" readonly><template #append><el-button @click="copy(webhookUrl)">复制</el-button></template></el-input></el-form-item>
        <p class="text-sm text-muted mb-3">每个账号在卡台填写同一地址，再将各自的 Webhook Secret 填入上方账号配置。不同账号必须使用不同密钥。</p>
        <el-form-item label="本机出口 IP（加入卡台 API 白名单）"><el-input :model-value="egress || '尚未检测'" readonly><template #append><el-button :loading="checkingNetwork" @click="loadNetwork">检测</el-button></template></el-input></el-form-item>
        <el-form-item label="代理换码密码（全站共用）"><el-input v-model="swapPassword" type="password" show-password :placeholder="swapConfigured ? '已设置，留空不修改' : '至少 6 位'" autocomplete="new-password" /></el-form-item>
        <el-form-item label="代理换码链接"><el-input :model-value="swapUrl" readonly><template #append><el-button @click="copy(swapUrl)">复制</el-button></template></el-input></el-form-item>
        <el-button type="primary" :loading="savingSettings" @click="saveSettings">保存共用设置</el-button>
      </el-form>
    </section>
    <el-dialog v-model="showEditor" :title="form.id ? '编辑账号' : '新增卡台账号'" width="520px" :close-on-click-modal="false">
      <el-form label-position="top" @submit.prevent>
        <el-form-item label="账号名称"><el-input v-model="form.name" maxlength="80" placeholder="例如：主账号、备用资金账号" /></el-form-item>
        <el-form-item label="API Key"><el-input v-model="form.api_key" type="password" show-password :disabled="keyLocked" :placeholder="keyLocked ? '已配置，保留原 Key' : '粘贴该账号的 API Key'" autocomplete="new-password" /></el-form-item>
        <p class="text-xs text-muted mb-4">API Key 确定卡密归属，保存后不能替换成另一账号。使用新的 Key 请新增账号。</p>
        <el-form-item label="Webhook Secret"><el-input v-model="form.webhook_secret" type="password" show-password :placeholder="secretConfigured ? '已配置，留空保留' : '粘贴该账号回调密钥（可稍后填写）'" autocomplete="new-password" /></el-form-item>
        <p class="text-xs text-muted mb-3">更换回调密钥后，旧密钥保留 24 小时验证在途通知；新账号不能使用其他账号的历史密钥。</p>
        <el-alert v-if="formError" :title="formError" type="error" :closable="false" />
      </el-form>
      <template #footer><el-button :disabled="saving" @click="showEditor=false">取消</el-button><el-button type="primary" :loading="saving" @click="saveAccount">保存账号</el-button></template>
    </el-dialog>
  </div>
</template>
<script setup lang="ts">
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import { authFetch } from '../../lib/api'
import type { CardAccount } from '../../lib/cardAccounts'
import { dialog } from '../../lib/dialog'
const accounts = ref<CardAccount[]>([])
const balances = reactive<Record<number, { spendable?: string; total?: string; reserve?: string; time?: string; error?: string; loading?: boolean }>>({})
const base = ref('https://zovocard.com'), egress = ref(''), error = ref(''), swapPassword = ref('')
const webhookUrl = `${location.origin}/api/v1/webhooks/cardplatform`, swapUrl = `${location.origin}/partner/swap`
const refreshing = ref(false), checkingNetwork = ref(false), savingSettings = ref(false), swapConfigured = ref(false)
const showEditor = ref(false), saving = ref(false), keyLocked = ref(false), secretConfigured = ref(false), formError = ref('')
const form = reactive({ id: 0, name: '', api_key: '', webhook_secret: '' })
const controller = new AbortController()
onUnmounted(() => controller.abort())
async function request(url: string, init: RequestInit = {}) {
  const r = await authFetch(url, { ...init, signal: controller.signal })
  const d = await r.json()
  if (!r.ok) throw new Error(d.error || '请求失败')
  return d
}
async function load() {
  error.value = ''
  try {
    const d = await request('/api/v1/admin/cardplatform/accounts'); accounts.value = d.accounts || []; base.value = d.site_base
    const settings = await request('/api/v1/admin/settings'); swapConfigured.value = !!settings.agent_swap_password_configured
  } catch (e: any) { error.value = e.message }
}
function edit(a?: CardAccount) {
  Object.assign(form, { id: a?.id || 0, name: a?.name || '', api_key: '', webhook_secret: '' })
  keyLocked.value = !!a?.api_key_configured; secretConfigured.value = !!a?.webhook_secret_configured
  formError.value = ''; showEditor.value = true
}
async function saveAccount() {
  saving.value = true; formError.value = ''
  try {
    const saved = await request(`/api/v1/admin/cardplatform/accounts${form.id ? `/${form.id}` : ''}`, { method: form.id ? 'PUT' : 'POST', body: JSON.stringify(form) })
    showEditor.value = false; form.api_key = ''; form.webhook_secret = ''
    await load()
    const account = accounts.value.find(a => a.id === saved.id)
    if (account) void refreshBalance(account)
    dialog.toast(saved.rules_synced ? '账号已保存，共用选卡规则已同步' : `账号已保存，但规则同步失败：${saved.rules_error}。请到选卡配置重新同步。`, saved.rules_synced ? 'ok' : 'warn')
  } catch (e: any) { formError.value = e.message }
  finally { saving.value = false }
}
async function refreshBalance(a: CardAccount) {
  if (balances[a.id]?.loading) return
  balances[a.id] = { ...balances[a.id], loading: true }
  try {
    const d = await request('/api/v1/admin/cardplatform/balance', { headers: { 'X-Card-Account-ID': String(a.id) } })
    balances[a.id] = { spendable: d.spendable_balance == null ? undefined : String(d.spendable_balance), total: d.balance == null ? undefined : String(d.balance), reserve: d.account_reserve_amount == null ? undefined : String(d.account_reserve_amount), time: new Date().toLocaleString() }
  } catch (e: any) { balances[a.id] = { ...balances[a.id], error: e.message } }
  finally { balances[a.id].loading = false }
}
async function refreshAll() {
  refreshing.value = true
  try {
    for (let i = 0; i < accounts.value.length; i += 3) {
      if (controller.signal.aborted) break
      await Promise.allSettled(accounts.value.slice(i, i + 3).map(refreshBalance))
    }
  }
  finally { refreshing.value = false }
}
async function loadNetwork() {
  checkingNetwork.value = true
  try { const d = await request('/api/v1/admin/network/egress'); egress.value = d.egress_ip || '' }
  catch (e: any) { error.value = e.message }
  finally { checkingNetwork.value = false }
}
async function saveSettings() {
  savingSettings.value = true
  try {
    await request('/api/v1/admin/settings', { method: 'PUT', body: JSON.stringify({ card_api_base: base.value, agent_swap_password: swapPassword.value }) })
    swapPassword.value = ''; await load(); dialog.toast('共用设置已保存', 'ok')
  } catch (e: any) { error.value = e.message }
  finally { savingSettings.value = false }
}
async function copy(text: string) { try { await navigator.clipboard.writeText(text); dialog.toast('已复制', 'ok') } catch { dialog.toast('复制失败', 'err') } }
onMounted(async () => { await load(); void refreshAll() })
</script>
