<script setup lang="ts">
import { keysApi } from "@/api/keys";
import type { APIKey, Group, KeyRuntimeStatus, KeyStatus } from "@/types/models";
import { appState, triggerSyncOperationRefresh } from "@/utils/app-state";
import { copy } from "@/utils/clipboard";
import { getGroupDisplayName, maskKey } from "@/utils/display";
import {
  AddCircleOutline,
  AlertCircleOutline,
  CheckmarkCircle,
  CopyOutline,
  EyeOffOutline,
  EyeOutline,
  Pencil,
  RemoveCircleOutline,
  Search,
} from "@vicons/ionicons5";
import {
  NButton,
  NDropdown,
  NEmpty,
  NIcon,
  NInput,
  NInputNumber,
  NModal,
  NSelect,
  NSpace,
  NSpin,
  useDialog,
  type MessageReactive,
} from "naive-ui";
import { h, onMounted, onUnmounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import KeyCreateDialog from "./KeyCreateDialog.vue";
import KeyDeleteDialog from "./KeyDeleteDialog.vue";

const { t } = useI18n();

interface KeyRow extends APIKey {
  is_visible: boolean;
}

interface Props {
  selectedGroup: Group | null;
}

const props = defineProps<Props>();

const keys = ref<KeyRow[]>([]);
const loading = ref(false);
const searchText = ref("");
const statusFilter = ref<"all" | "active" | "invalid">("all");
const currentPage = ref(1);
const pageSize = ref(12);
const total = ref(0);
const totalPages = ref(0);
const dialog = useDialog();
const confirmInput = ref("");
const cooldownTick = ref(0);
let cooldownTimer: ReturnType<typeof setInterval> | null = null;

onMounted(() => {
  cooldownTimer = setInterval(() => { cooldownTick.value++; }, 1000);
});
onUnmounted(() => {
  if (cooldownTimer) { clearInterval(cooldownTimer); cooldownTimer = null; }
});

// 状态过滤选项
const statusOptions = [
  { label: t("common.all"), value: "all" },
  { label: t("keys.valid"), value: "active" },
  { label: t("keys.invalid"), value: "invalid" },
];

// 更多操作下拉菜单选项
const moreOptions = [
  { label: t("keys.exportAllKeys"), key: "copyAll" },
  { label: t("keys.exportValidKeys"), key: "copyValid" },
  { label: t("keys.exportInvalidKeys"), key: "copyInvalid" },
  { type: "divider" },
  { label: t("keys.restoreAllInvalidKeys"), key: "restoreAll" },
  {
    label: t("keys.clearAllInvalidKeys"),
    key: "clearInvalid",
    props: { style: { color: "#d03050" } },
  },
  {
    label: t("keys.clearAllKeys"),
    key: "clearAll",
    props: { style: { color: "red", fontWeight: "bold" } },
  },
  { type: "divider" },
  { label: t("keys.validateAllKeys"), key: "validateAll" },
  { label: t("keys.validateValidKeys"), key: "validateActive" },
  { label: t("keys.validateInvalidKeys"), key: "validateInvalid" },
];

let testingMsg: MessageReactive | null = null;
const isDeling = ref(false);
const isRestoring = ref(false);

const createDialogShow = ref(false);
const deleteDialogShow = ref(false);

// 备注编辑相关
const notesDialogShow = ref(false);
const editingKey = ref<KeyRow | null>(null);
const editingNotes = ref("");

// 优先级编辑相关
const priorityDialogShow = ref(false);
const editingPriority = ref(0);

watch(
  () => props.selectedGroup,
  async newGroup => {
    if (newGroup) {
      // 检查重置页面是否会触发分页观察者。
      const willWatcherTrigger = currentPage.value !== 1 || statusFilter.value !== "all";
      resetPage();
      // 如果分页观察者不触发，则手动加载。
      if (!willWatcherTrigger) {
        await loadKeys();
      }
    }
  },
  { immediate: true }
);

watch([currentPage, pageSize], async () => {
  await loadKeys();
});

watch(statusFilter, async () => {
  if (currentPage.value !== 1) {
    currentPage.value = 1;
  } else {
    await loadKeys();
  }
});

// 监听任务完成事件，自动刷新密钥列表
watch(
  () => appState.groupDataRefreshTrigger,
  () => {
    // 检查是否需要刷新当前分组的密钥列表
    if (appState.lastCompletedTask && props.selectedGroup) {
      // 通过分组名称匹配
      const isCurrentGroup = appState.lastCompletedTask.groupName === props.selectedGroup.name;

      const shouldRefresh =
        appState.lastCompletedTask.taskType === "KEY_VALIDATION" ||
        appState.lastCompletedTask.taskType === "KEY_IMPORT" ||
        appState.lastCompletedTask.taskType === "KEY_DELETE";

      if (isCurrentGroup && shouldRefresh) {
        // 刷新当前分组的密钥列表
        loadKeys();
      }
    }
  }
);

// 处理搜索输入的防抖
function handleSearchInput() {
  if (currentPage.value !== 1) {
    currentPage.value = 1;
  } else {
    loadKeys();
  }
}

// 处理更多操作菜单
function handleMoreAction(key: string) {
  switch (key) {
    case "copyAll":
      copyAllKeys();
      break;
    case "copyValid":
      copyValidKeys();
      break;
    case "copyInvalid":
      copyInvalidKeys();
      break;
    case "restoreAll":
      restoreAllInvalid();
      break;
    case "validateAll":
      validateKeys("all");
      break;
    case "validateActive":
      validateKeys("active");
      break;
    case "validateInvalid":
      validateKeys("invalid");
      break;
    case "clearInvalid":
      clearAllInvalid();
      break;
    case "clearAll":
      clearAll();
      break;
  }
}

async function loadKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  try {
    loading.value = true;
    const result = await keysApi.getGroupKeys({
      group_id: props.selectedGroup.id,
      page: currentPage.value,
      page_size: pageSize.value,
      status: statusFilter.value === "all" ? undefined : (statusFilter.value as KeyStatus),
      key_value: searchText.value.trim() || undefined,
    });
    keys.value = result.items as KeyRow[];
    total.value = result.pagination.total_items;
    totalPages.value = result.pagination.total_pages;
  } finally {
    loading.value = false;
  }
}

// 处理批量删除成功后的刷新
async function handleBatchDeleteSuccess() {
  await loadKeys();
  // 触发同步操作刷新
  if (props.selectedGroup) {
    triggerSyncOperationRefresh(props.selectedGroup.name, "BATCH_DELETE");
  }
}

async function copyKey(key: KeyRow) {
  const success = await copy(key.key_value);
  if (success) {
    window.$message.success(t("keys.keyCopied"));
  } else {
    window.$message.error(t("keys.copyFailed"));
  }
}

async function testKey(_key: KeyRow) {
  if (!props.selectedGroup?.id || !_key.key_value || testingMsg) {
    return;
  }

  testingMsg = window.$message.info(t("keys.testingKey"), {
    duration: 0,
  });

  try {
    const response = await keysApi.testKeys(props.selectedGroup.id, _key.key_value);
    const curValid = response.results?.[0] || {};
    if (curValid.is_valid) {
      window.$message.success(
        t("keys.testSuccess", { duration: formatDuration(response.total_duration) })
      );
    } else {
      window.$message.error(curValid.error || t("keys.testFailed"), {
        keepAliveOnHover: true,
        closable: true,
      });
    }
    await loadKeys();
    // 触发同步操作刷新
    triggerSyncOperationRefresh(props.selectedGroup.name, "TEST_SINGLE");
  } catch (_error) {
    console.error("Test failed");
  } finally {
    testingMsg?.destroy();
    testingMsg = null;
  }
}

function formatDuration(ms: number): string {
  if (ms < 0) {
    return "0ms";
  }

  const minutes = Math.floor(ms / 60000);
  const seconds = Math.floor((ms % 60000) / 1000);
  const milliseconds = ms % 1000;

  let result = "";
  if (minutes > 0) {
    result += `${minutes}m`;
  }
  if (seconds > 0) {
    result += `${seconds}s`;
  }
  if (milliseconds > 0 || result === "") {
    result += `${milliseconds}ms`;
  }

  return result;
}

function toggleKeyVisibility(key: KeyRow) {
  key.is_visible = !key.is_visible;
}

// 获取要显示的值（备注优先，否则显示密钥）
function getDisplayValue(key: KeyRow): string {
  if (key.notes && !key.is_visible) {
    return key.notes;
  }
  return key.is_visible ? key.key_value : maskKey(key.key_value);
}

// 编辑密钥备注
function editKeyNotes(key: KeyRow) {
  editingKey.value = key;
  editingNotes.value = key.notes || "";
  notesDialogShow.value = true;
}

// 保存备注
async function saveKeyNotes() {
  if (!editingKey.value) {
    return;
  }

  try {
    const trimmed = editingNotes.value.trim();
    await keysApi.updateKeyNotes(editingKey.value.id, trimmed);
    editingKey.value.notes = trimmed;
    window.$message.success(t("keys.notesUpdated"));
    notesDialogShow.value = false;
  } catch (error) {
    console.error("Update notes failed", error);
  }
}

// 编辑密钥优先级
function editKeyPriority(key: KeyRow) {
  editingKey.value = key;
  editingPriority.value = key.priority || 0;
  priorityDialogShow.value = true;
}

// 保存优先级
async function saveKeyPriority() {
  if (!editingKey.value) {
    return;
  }

  try {
    const priority = Math.max(0, Math.min(100, editingPriority.value));
    await keysApi.updateKeyPriority(editingKey.value.id, priority);
    editingKey.value.priority = priority;
    window.$message.success(t("keys.priorityUpdated"));
    priorityDialogShow.value = false;
    await loadKeys();
  } catch (error) {
    console.error("Update priority failed", error);
  }
}

// 切换密钥手动禁用状态
async function toggleManuallyDisabled(key: KeyRow) {
  if (!props.selectedGroup?.id) {
    return;
  }

  const willDisable = !key.is_manually_disabled;
  const d = dialog.warning({
    title: willDisable ? t("keys.disableKey") : t("keys.enableKey"),
    content: willDisable
      ? t("keys.confirmDisableKey", { key: maskKey(key.key_value) })
      : t("keys.confirmEnableKey", { key: maskKey(key.key_value) }),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      d.loading = true;

      try {
        await keysApi.setKeyManuallyDisabled(props.selectedGroup.id, [key.id], willDisable);
        window.$message.success(willDisable ? t("keys.keyDisabled") : t("keys.keyEnabled"));
        await loadKeys();
        triggerSyncOperationRefresh(props.selectedGroup.name, "TOGGLE_DISABLED");
      } catch (error) {
        console.error("Toggle manually disabled failed", error);
      } finally {
        d.loading = false;
      }
    },
  });
}

// 清除密钥冷却状态
async function clearKeyCooldown(key: KeyRow) {
  if (!props.selectedGroup?.id) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.clearCooldown"),
    content: t("keys.confirmClearCooldown", { key: maskKey(key.key_value) }),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      d.loading = true;

      try {
        await keysApi.clearCooldown(props.selectedGroup.id, [key.id]);
        window.$message.success(t("keys.cooldownCleared"));
        await loadKeys();
        triggerSyncOperationRefresh(props.selectedGroup.name, "CLEAR_COOLDOWN");
      } catch (error) {
        console.error("Clear cooldown failed", error);
      } finally {
        d.loading = false;
      }
    },
  });
}


async function restoreKey(key: KeyRow) {
  if (!props.selectedGroup?.id || !key.key_value || isRestoring.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.restoreKey"),
    content: t("keys.confirmRestoreKey", { key: maskKey(key.key_value) }),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      isRestoring.value = true;
      d.loading = true;

      try {
        await keysApi.restoreKeys(props.selectedGroup.id, key.key_value);
        await loadKeys();
        // 触发同步操作刷新
        triggerSyncOperationRefresh(props.selectedGroup.name, "RESTORE_SINGLE");
      } catch (_error) {
        console.error("Restore failed");
      } finally {
        d.loading = false;
        isRestoring.value = false;
      }
    },
  });
}

async function deleteKey(key: KeyRow) {
  if (!props.selectedGroup?.id || !key.key_value || isDeling.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.deleteKey"),
    content: t("keys.confirmDeleteKey", { key: maskKey(key.key_value) }),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      d.loading = true;
      isDeling.value = true;

      try {
        await keysApi.deleteKeys(props.selectedGroup.id, key.key_value);
        await loadKeys();
        // 触发同步操作刷新
        triggerSyncOperationRefresh(props.selectedGroup.name, "DELETE_SINGLE");
      } catch (_error) {
        console.error("Delete failed");
      } finally {
        d.loading = false;
        isDeling.value = false;
      }
    },
  });
}

function formatRelativeTime(date: string) {
  if (!date) {
    return t("keys.never");
  }
  const now = new Date();
  const target = new Date(date);
  const diffSeconds = Math.floor((now.getTime() - target.getTime()) / 1000);
  const diffMinutes = Math.floor(diffSeconds / 60);
  const diffHours = Math.floor(diffMinutes / 60);
  const diffDays = Math.floor(diffHours / 24);

  if (diffDays > 0) {
    return t("keys.daysAgo", { days: diffDays });
  }
  if (diffHours > 0) {
    return t("keys.hoursAgo", { hours: diffHours });
  }
  if (diffMinutes > 0) {
    return t("keys.minutesAgo", { minutes: diffMinutes });
  }
  if (diffSeconds > 0) {
    return t("keys.secondsAgo", { seconds: diffSeconds });
  }
  return t("keys.justNow");
}

function getRuntimeStatus(key: KeyRow): KeyRuntimeStatus {
  return key.runtime_status || (key.status as KeyRuntimeStatus);
}

function getStatusClass(key: KeyRow): string {
  switch (getRuntimeStatus(key)) {
    case "cooling":
      return "status-cooling";
    case "auto_disabled":
      return "status-auto-disabled";
    case "invalid":
      return "status-invalid";
    case "active":
      return "status-valid";
    default:
      return "status-unknown";
  }
}

function getStatusTagType(key: KeyRow): "success" | "warning" | "error" | "default" {
  switch (getRuntimeStatus(key)) {
    case "cooling":
      return "warning";
    case "auto_disabled":
      return "error";
    case "invalid":
      return "default";
    default:
      return "success";
  }
}

function getStatusTagText(key: KeyRow): string {
  switch (getRuntimeStatus(key)) {
    case "cooling":
      return t("keys.coolingShort");
    case "auto_disabled":
      return t("keys.autoDisabledShort");
    case "invalid":
      return t("keys.invalidShort");
    default:
      return t("keys.validShort");
  }
}

function parseUpstreamErrorMessage(raw: string): string {
  if (!raw) return "";
  try {
    const parsed = JSON.parse(raw);
    // Standard format: {"error": {"message": "...", "type": "...", "code": "..."}}
    if (parsed.error?.message) {
      const parts = [parsed.error.message];
      if (parsed.error.type) parts.push(`[${parsed.error.type}]`);
      if (parsed.error.code) parts.push(`[${parsed.error.code}]`);
      return parts.join(" ");
    }
    // Vendor format: {"error_msg": "..."}
    if (parsed.error_msg) return parsed.error_msg;
    // Simple format: {"error": "..."}
    if (typeof parsed.error === "string") return parsed.error;
    // Root message: {"message": "..."}
    if (parsed.message) return parsed.message;
    return raw;
  } catch {
    return raw;
  }
}

function getStatusReason(key: KeyRow): string {
  const parts: string[] = [];
  if (key.last_error_code > 0) {
    parts.push(`${t("keys.statusCodeLabel")}: ${key.last_error_code}`);
  }
  const raw = key.status_reason || key.last_error_message;
  if (raw) {
    parts.push(parseUpstreamErrorMessage(raw));
  }
  return parts.join(" · ");
}

function shouldShowStatusReason(key: KeyRow): boolean {
  return getRuntimeStatus(key) !== "active" && getStatusReason(key).length > 0;
}

function formatCooldownRemaining(seconds?: number): string {
  if (!seconds || seconds <= 0) {
    return t("keys.cooldownRemaining", { seconds: 0 });
  }
  return t("keys.cooldownRemaining", { seconds });
}

function getCooldownRemaining(key: KeyRow): number {
  if (!key.cooldown_until) return key.cooldown_remaining_seconds || 0;
  void cooldownTick.value;
  const remaining = Math.max(0, Math.ceil((new Date(key.cooldown_until).getTime() - Date.now()) / 1000));
  if (remaining === 0 && key.runtime_status === "cooling") {
    key.runtime_status = "active";
  }
  return remaining;
}

async function copyAllKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  keysApi.exportKeys(props.selectedGroup.id, "all");
}

async function copyValidKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  keysApi.exportKeys(props.selectedGroup.id, "active");
}

async function copyInvalidKeys() {
  if (!props.selectedGroup?.id) {
    return;
  }

  keysApi.exportKeys(props.selectedGroup.id, "invalid");
}

async function restoreAllInvalid() {
  if (!props.selectedGroup?.id || isRestoring.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.restoreKeys"),
    content: t("keys.confirmRestoreAllInvalid"),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      isRestoring.value = true;
      d.loading = true;
      try {
        await keysApi.restoreAllInvalidKeys(props.selectedGroup.id);
        await loadKeys();
        // 触发同步操作刷新
        triggerSyncOperationRefresh(props.selectedGroup.name, "RESTORE_ALL_INVALID");
      } catch (_error) {
        console.error("Restore failed");
      } finally {
        d.loading = false;
        isRestoring.value = false;
      }
    },
  });
}

async function validateKeys(status: "all" | "active" | "invalid") {
  if (!props.selectedGroup?.id || testingMsg) {
    return;
  }

  let statusText = t("common.all");
  if (status === "active") {
    statusText = t("keys.valid");
  } else if (status === "invalid") {
    statusText = t("keys.invalid");
  }

  testingMsg = window.$message.info(t("keys.validatingKeysMsg", { type: statusText }), {
    duration: 0,
  });

  try {
    await keysApi.validateGroupKeys(props.selectedGroup.id, status === "all" ? undefined : status);
    localStorage.removeItem("last_closed_task");
    appState.taskPollingTrigger++;
  } catch (_error) {
    console.error("Test failed");
  } finally {
    testingMsg?.destroy();
    testingMsg = null;
  }
}

async function clearAllInvalid() {
  if (!props.selectedGroup?.id || isDeling.value) {
    return;
  }

  const d = dialog.warning({
    title: t("keys.clearKeys"),
    content: t("keys.confirmClearInvalidKeys"),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: async () => {
      if (!props.selectedGroup?.id) {
        return;
      }

      isDeling.value = true;
      d.loading = true;
      try {
        const { data } = await keysApi.clearAllInvalidKeys(props.selectedGroup.id);
        window.$message.success(data?.message || t("keys.clearSuccess"));
        await loadKeys();
        // 触发同步操作刷新
        triggerSyncOperationRefresh(props.selectedGroup.name, "CLEAR_ALL_INVALID");
      } catch (_error) {
        console.error("Delete failed");
      } finally {
        d.loading = false;
        isDeling.value = false;
      }
    },
  });
}

async function clearAll() {
  if (!props.selectedGroup?.id || isDeling.value) {
    return;
  }

  dialog.warning({
    title: t("keys.clearAllKeys"),
    content: t("keys.confirmClearAllKeys"),
    positiveText: t("common.confirm"),
    negativeText: t("common.cancel"),
    onPositiveClick: () => {
      confirmInput.value = ""; // Reset before opening second dialog
      dialog.create({
        title: t("keys.enterGroupNameToConfirm"),
        content: () =>
          h("div", null, [
            h("p", null, [
              t("keys.dangerousOperationWarning1"),
              h("strong", null, t("common.all")),
              t("keys.dangerousOperationWarning2"),
              h("strong", { style: { color: "#d03050" } }, props.selectedGroup?.name),
              t("keys.toConfirm"),
            ]),
            h(NInput, {
              value: confirmInput.value,
              "onUpdate:value": v => {
                confirmInput.value = v;
              },
              placeholder: t("keys.enterGroupName"),
            }),
          ]),
        positiveText: t("keys.confirmClear"),
        negativeText: t("common.cancel"),
        onPositiveClick: async () => {
          if (confirmInput.value !== props.selectedGroup?.name) {
            window.$message.error(t("keys.incorrectGroupName"));
            return false; // Prevent dialog from closing
          }

          if (!props.selectedGroup?.id) {
            return;
          }

          isDeling.value = true;
          try {
            await keysApi.clearAllKeys(props.selectedGroup.id);
            window.$message.success(t("keys.clearAllKeysSuccess"));
            await loadKeys();
            // Trigger sync operation refresh
            triggerSyncOperationRefresh(props.selectedGroup.name, "CLEAR_ALL");
          } catch (_error) {
            console.error("Clear all failed", _error);
          } finally {
            isDeling.value = false;
          }
        },
      });
    },
  });
}

function changePage(page: number) {
  currentPage.value = page;
}

function changePageSize(size: number) {
  pageSize.value = size;
  currentPage.value = 1;
}

function resetPage() {
  currentPage.value = 1;
  searchText.value = "";
  statusFilter.value = "all";
}
</script>

<template>
  <div class="key-table-container">
    <!-- 工具栏 -->
    <div class="toolbar">
      <div class="toolbar-left">
        <n-button type="success" size="small" @click="createDialogShow = true">
          <template #icon>
            <n-icon :component="AddCircleOutline" />
          </template>
          {{ t("keys.addKey") }}
        </n-button>
        <n-button type="error" size="small" @click="deleteDialogShow = true">
          <template #icon>
            <n-icon :component="RemoveCircleOutline" />
          </template>
          {{ t("keys.deleteKey") }}
        </n-button>
      </div>
      <div class="toolbar-right">
        <n-space :size="12" align="center">
          <n-select
            v-model:value="statusFilter"
            :options="statusOptions"
            size="small"
            style="width: 120px"
            :placeholder="t('keys.allStatus')"
          />
          <n-input-group>
            <n-input
              v-model:value="searchText"
              :placeholder="t('keys.keyExactMatch')"
              size="small"
              style="width: 200px"
              clearable
              @keyup.enter="handleSearchInput"
            >
              <template #prefix>
                <n-icon :component="Search" />
              </template>
            </n-input>
            <n-button
              type="primary"
              ghost
              size="small"
              :disabled="loading"
              @click="handleSearchInput"
            >
              {{ t("common.search") }}
            </n-button>
          </n-input-group>
          <n-dropdown :options="moreOptions" trigger="click" @select="handleMoreAction">
            <n-button size="small" tertiary>
              <template #icon>
                <span style="font-size: 16px; font-weight: bold">⋯</span>
              </template>
            </n-button>
          </n-dropdown>
        </n-space>
      </div>
    </div>

    <!-- 密钥卡片网格 -->
    <div class="keys-grid-container">
      <n-spin :show="loading">
        <div v-if="keys.length === 0 && !loading" class="empty-container">
          <n-empty :description="t('keys.noMatchingKeys')" />
        </div>
        <div v-else class="keys-grid">
          <div
            v-for="key in keys"
            :key="key.id"
            class="key-card"
            :class="getStatusClass(key)"
          >
            <!-- 主要信息行：Key + 快速操作 -->
            <div class="key-main">
              <div class="key-section">
                <div style="display: flex; align-items: center; gap: 8px;">
                  <n-tag :type="getStatusTagType(key)" :bordered="false" round>
                    <template #icon>
                      <n-icon
                        :component="getRuntimeStatus(key) === 'active' ? CheckmarkCircle : AlertCircleOutline"
                      />
                    </template>
                    {{ getStatusTagText(key) }}
                  </n-tag>
                  <n-tag
                    v-if="getRuntimeStatus(key) === 'cooling'"
                    type="warning"
                    :bordered="false"
                    size="small"
                    round
                  >
                    {{ formatCooldownRemaining(getCooldownRemaining(key)) }}
                  </n-tag>
                  <n-tag
                    v-if="key.is_manually_disabled"
                    type="error"
                    :bordered="false"
                    size="small"
                    round
                  >
                    {{ t("keys.manuallyDisabled") }}
                  </n-tag>
                  <n-tag
                    :bordered="false"
                    size="small"
                    style="cursor: pointer;"
                    @click="editKeyPriority(key)"
                    :title="t('keys.editPriority')"
                  >
                    {{ t("keys.priority") }}: {{ key.priority || 0 }}
                  </n-tag>
                </div>
                <n-input class="key-text" :value="getDisplayValue(key)" readonly size="small" />
                <div v-if="shouldShowStatusReason(key)" class="status-reason">
                  {{ getStatusReason(key) }}
                </div>
                <div class="quick-actions">
                  <n-button
                    size="tiny"
                    text
                    @click="editKeyNotes(key)"
                    :title="t('keys.editNotes')"
                  >
                    <template #icon>
                      <n-icon :component="Pencil" />
                    </template>
                  </n-button>
                  <n-button
                    size="tiny"
                    text
                    @click="toggleKeyVisibility(key)"
                    :title="t('keys.showHide')"
                  >
                    <template #icon>
                      <n-icon :component="key.is_visible ? EyeOffOutline : EyeOutline" />
                    </template>
                  </n-button>
                  <n-button size="tiny" text @click="copyKey(key)" :title="t('common.copy')">
                    <template #icon>
                      <n-icon :component="CopyOutline" />
                    </template>
                  </n-button>
                </div>
              </div>
            </div>

            <!-- 统计信息 + 操作按钮行 -->
            <div class="key-bottom">
              <div class="key-stats">
                <span class="stat-item">
                  {{ t("keys.requestsShort") }}
                  <strong>{{ key.request_count }}</strong>
                </span>
                <span class="stat-item">
                  {{ t("keys.failuresShort") }}
                  <strong>{{ key.failure_count }}</strong>
                </span>
                <span class="stat-item">
                  {{ key.last_used_at ? formatRelativeTime(key.last_used_at) : t("keys.unused") }}
                </span>
              </div>
              <n-button-group class="key-actions">
                <n-button
                  round
                  tertiary
                  type="info"
                  size="tiny"
                  @click="testKey(key)"
                  :title="t('keys.testKey')"
                >
                  {{ t("keys.testShort") }}
                </n-button>
                <n-button
                  v-if="key.status !== 'active'"
                  tertiary
                  size="tiny"
                  @click="restoreKey(key)"
                  :title="t('keys.restoreKey')"
                  type="warning"
                >
                  {{ t("keys.restoreShort") }}
                </n-button>
                <n-button
                  v-if="getRuntimeStatus(key) === 'cooling'"
                  tertiary
                  size="tiny"
                  @click="clearKeyCooldown(key)"
                  :title="t('keys.clearCooldown')"
                  type="success"
                >
                  {{ t("keys.clearCooldownShort") }}
                </n-button>
                <n-button
                  round
                  tertiary
                  size="tiny"
                  :type="key.is_manually_disabled ? 'success' : 'warning'"
                  @click="toggleManuallyDisabled(key)"
                  :title="key.is_manually_disabled ? t('keys.enableKey') : t('keys.disableKey')"
                >
                  {{ key.is_manually_disabled ? t("common.enable") : t("common.disable") }}
                </n-button>
                <n-button
                  round
                  tertiary
                  size="tiny"
                  type="error"
                  @click="deleteKey(key)"
                  :title="t('keys.deleteKey')"
                >
                  {{ t("common.deleteShort") }}
                </n-button>
              </n-button-group>
            </div>
          </div>
        </div>
      </n-spin>
    </div>

    <!-- 分页 -->
    <div class="pagination-container">
      <div class="pagination-info">
        <span>{{ t("keys.totalRecords", { total }) }}</span>
        <n-select
          v-model:value="pageSize"
          :options="[
            { label: t('keys.recordsPerPage', { count: 12 }), value: 12 },
            { label: t('keys.recordsPerPage', { count: 24 }), value: 24 },
            { label: t('keys.recordsPerPage', { count: 60 }), value: 60 },
            { label: t('keys.recordsPerPage', { count: 120 }), value: 120 },
          ]"
          size="small"
          style="width: 100px; margin-left: 12px"
          @update:value="changePageSize"
        />
      </div>
      <div class="pagination-controls">
        <n-button size="small" :disabled="currentPage <= 1" @click="changePage(currentPage - 1)">
          {{ t("common.previousPage") }}
        </n-button>
        <span class="page-info">
          {{ t("keys.pageInfo", { current: currentPage, total: totalPages }) }}
        </span>
        <n-button
          size="small"
          :disabled="currentPage >= totalPages"
          @click="changePage(currentPage + 1)"
        >
          {{ t("common.nextPage") }}
        </n-button>
      </div>
    </div>

    <key-create-dialog
      v-if="selectedGroup?.id"
      v-model:show="createDialogShow"
      :group-id="selectedGroup.id"
      :group-name="getGroupDisplayName(selectedGroup!)"
      @success="loadKeys"
    />

    <key-delete-dialog
      v-if="selectedGroup?.id"
      v-model:show="deleteDialogShow"
      :group-id="selectedGroup.id"
      :group-name="getGroupDisplayName(selectedGroup!)"
      @success="handleBatchDeleteSuccess"
    />
  </div>

  <!-- 备注编辑对话框 -->
  <n-modal v-model:show="notesDialogShow" preset="dialog" :title="t('keys.editKeyNotes')">
    <n-input
      v-model:value="editingNotes"
      type="textarea"
      :placeholder="t('keys.enterNotes')"
      :rows="3"
      maxlength="255"
      show-count
    />
    <template #action>
      <n-button @click="notesDialogShow = false">{{ t("common.cancel") }}</n-button>
      <n-button type="primary" @click="saveKeyNotes">{{ t("common.save") }}</n-button>
    </template>
  </n-modal>

  <!-- 优先级编辑对话框 -->
  <n-modal v-model:show="priorityDialogShow" preset="dialog" :title="t('keys.editPriority')">
    <n-input-number
      v-model:value="editingPriority"
      :placeholder="t('keys.enterPriority')"
      :min="0"
      :max="100"
      style="width: 100%"
    />
    <template #action>
      <n-button @click="priorityDialogShow = false">{{ t("common.cancel") }}</n-button>
      <n-button type="primary" @click="saveKeyPriority">{{ t("common.save") }}</n-button>
    </template>
  </n-modal>
</template>

<style scoped>
.key-table-container {
  background: var(--card-bg-solid);
  border-radius: 8px;
  box-shadow: var(--shadow-md);
  border: 1px solid var(--border-color);
  overflow: hidden;
  height: 100%;
  display: flex;
  flex-direction: column;
}

.toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 16px;
  background: var(--card-bg-solid);
  border-bottom: 1px solid var(--border-color);
  flex-shrink: 0;
  gap: 16px;
  min-height: 64px;
}

.toolbar :deep(.n-button) {
  font-weight: 500;
}

.toolbar-left {
  display: flex;
  gap: 8px;
  flex-shrink: 0;
}

.toolbar-right {
  display: flex;
  gap: 12px;
  align-items: center;
  flex: 1;
  justify-content: flex-end;
  min-width: 0;
}

.filter-group {
  display: flex;
  align-items: center;
  gap: 8px;
}

.more-actions {
  position: relative;
}

.more-menu {
  position: absolute;
  top: 100%;
  right: 0;
  background: var(--card-bg-solid);
  border: 1px solid var(--border-color);
  border-radius: 6px;
  box-shadow: var(--shadow-lg);
  min-width: 180px;
  z-index: 1000;
  overflow: hidden;
}

.menu-item {
  display: block;
  width: 100%;
  padding: 8px 12px;
  border: none;
  background: none;
  text-align: left;
  cursor: pointer;
  font-size: 14px;
  color: #333;
  transition: background-color 0.2s;
}

.menu-item:hover {
  background: #f8f9fa;
}

.menu-item.danger {
  color: #dc3545;
}

.menu-item.danger:hover {
  background: #f8d7da;
}

.menu-divider {
  height: 1px;
  background: #e9ecef;
  margin: 4px 0;
}

.btn {
  padding: 6px 12px;
  border: none;
  border-radius: 4px;
  cursor: pointer;
  font-size: 14px;
  transition: all 0.2s;
  white-space: nowrap;
}

.btn-sm {
  padding: 4px 8px;
  font-size: 12px;
}

.btn:disabled {
  opacity: 0.6;
  cursor: not-allowed;
}

.btn-primary {
  background: #007bff;
  color: white;
}

.btn-primary:hover:not(:disabled) {
  background: #0056b3;
}

.btn-secondary {
  background: #6c757d;
  color: white;
}

.btn-secondary:hover:not(:disabled) {
  background: #545b62;
}

.more-icon {
  font-size: 16px;
  font-weight: bold;
}

.filter-select,
.search-input,
.page-size-select {
  padding: 4px 8px;
  border: 1px solid #ced4da;
  border-radius: 4px;
  font-size: 12px;
}

.search-input {
  width: 180px;
}

.filter-select:focus,
.search-input:focus,
.page-size-select:focus {
  outline: none;
  border-color: #007bff;
  box-shadow: 0 0 0 2px rgba(0, 123, 255, 0.25);
}

/* 密钥卡片网格 */
.keys-grid-container {
  flex: 1;
  overflow-y: auto;
  padding: 16px;
}

.keys-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 16px;
}

.key-card {
  background: var(--card-bg-solid);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  padding: 14px;
  transition: all 0.2s;
  display: flex;
  flex-direction: column;
  gap: 10px;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.05);
}

.key-card:hover {
  box-shadow: var(--shadow-md);
  transform: translateY(-1px);
}

/* 状态相关样式 */
.key-card.status-valid {
  border-color: var(--success-border);
  background: var(--success-bg);
  border-width: 1.5px;
}

.key-card.status-invalid {
  border-color: var(--invalid-border);
  background: var(--card-bg-solid);
  opacity: 0.85;
}

.key-card.status-cooling {
  border-color: #f0a020;
  background: rgba(240, 160, 32, 0.08);
}

.key-card.status-auto-disabled {
  border-color: var(--error-border);
  background: var(--error-bg);
}

.key-card.status-error {
  border-color: var(--error-border);
  background: var(--error-bg);
}

/* 主要信息行 */
.key-main {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
}

.key-section {
  display: flex;
  flex-direction: column;
  align-items: stretch;
  gap: 8px;
  flex: 1;
  min-width: 0;
}

/* 底部统计和按钮行 */
.key-bottom {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
}

.key-stats {
  display: flex;
  gap: 8px;
  font-size: 12px;
  overflow: hidden;
  color: var(--text-secondary);
  flex: 1;
  min-width: 0;
}

.stat-item {
  white-space: nowrap;
  color: var(--text-secondary);
}

.stat-item strong {
  color: var(--text-primary);
  font-weight: 600;
}

.key-actions {
  flex-shrink: 0;
  &:deep(.n-button) {
    padding: 0 4px;
  }
}

.key-text {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, Courier, monospace;
  font-weight: 500;
  flex: 1;
  min-width: 0;
  overflow: hidden;
  white-space: nowrap;
}

/* 浅色主题 */
:root:not(.dark) .key-text {
  color: #495057;
  background: #f8f9fa;
}

/* 暗黑主题 */
:root.dark .key-text {
  color: var(--text-primary);
  background: var(--bg-tertiary);
}

:deep(.n-input__input-el) {
  font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, Courier, monospace;
  font-size: 13px;
}

.quick-actions {
  display: flex;
  gap: 4px;
  justify-content: flex-end;
  flex-shrink: 0;
}

.status-reason {
  font-size: 12px;
  line-height: 1.5;
  color: var(--text-secondary);
  word-break: break-word;
}

.quick-btn {
  padding: 4px 6px;
  border: none;
  background: transparent;
  cursor: pointer;
  border-radius: 3px;
  font-size: 12px;
  transition: background-color 0.2s;
}

/* 浅色主题 */
:root:not(.dark) .quick-btn:hover {
  background: #e9ecef;
}

/* 暗黑主题 */
:root.dark .quick-btn:hover {
  background: var(--bg-tertiary);
}

/* 统计信息行 */

.action-btn {
  padding: 2px 6px;
  border: 1px solid var(--border-color);
  background: var(--card-bg-solid);
  border-radius: 3px;
  cursor: pointer;
  font-size: 10px;
  font-weight: 500;
  transition: all 0.2s;
  white-space: nowrap;
  color: var(--text-primary);
}

.action-btn:hover {
  background: var(--bg-secondary);
}

.action-btn.primary {
  border-color: var(--primary-color);
  color: var(--primary-color);
}

.action-btn.primary:hover {
  background: var(--primary-color);
  color: white;
}

.action-btn.secondary {
  border-color: #6c757d;
  color: #6c757d;
}

.action-btn.secondary:hover {
  background: #6c757d;
  color: white;
}

.action-btn.danger {
  border-color: #dc3545;
  color: #dc3545;
}

.action-btn.danger:hover {
  background: #dc3545;
  color: white;
}

/* 加载和空状态 */
.loading-state,
.empty-state {
  display: flex;
  justify-content: center;
  align-items: center;
  height: 200px;
  color: #6c757d;
}

.loading-spinner {
  font-size: 14px;
}

.empty-text {
  font-size: 14px;
}

/* 分页 */
.pagination-container {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 12px 16px;
  background: var(--card-bg-solid);
  border-top: 1px solid var(--border-color);
  flex-shrink: 0;
  border-radius: 0 0 8px 8px;
}

.pagination-info {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 12px;
  color: var(--text-secondary);
}

.pagination-controls {
  display: flex;
  align-items: center;
  gap: 12px;
}

.page-info {
  font-size: 12px;
  color: var(--text-secondary);
}

@media (max-width: 768px) {
  .toolbar {
    flex-direction: column;
    align-items: stretch;
    gap: 12px;
  }

  .toolbar-left,
  .toolbar-right {
    width: 100%;
    justify-content: space-between;
  }

  .toolbar-right .n-space {
    width: 100%;
    justify-content: space-between;
  }
}
</style>
