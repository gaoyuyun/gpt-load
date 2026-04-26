package keypool

import (
	"errors"
	"fmt"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/store"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

type KeyProvider struct {
	db              *gorm.DB
	store           store.Store
	settingsManager *config.SystemSettingsManager
	encryptionSvc   encryption.Service
}

// NewProvider 创建一个新的 KeyProvider 实例。
func NewProvider(db *gorm.DB, store store.Store, settingsManager *config.SystemSettingsManager, encryptionSvc encryption.Service) *KeyProvider {
	return &KeyProvider{
		db:              db,
		store:           store,
		settingsManager: settingsManager,
		encryptionSvc:   encryptionSvc,
	}
}

// SelectKey 为指定的分组原子性地选择并轮换一个可用的 APIKey。
func (p *KeyProvider) SelectKey(groupID uint, group *models.Group, clientIdentifier string) (*models.APIKey, error) {
	cfg := group.EffectiveConfig
	strategy := cfg.KeySelectionStrategy

	// 1. 如果是 sticky 策略，尝试获取上次成功的 key
	if strategy == "sticky" && clientIdentifier != "" {
		lastKeyID, err := p.getLastUsedKey(groupID, clientIdentifier)
		if err == nil && lastKeyID > 0 {
			// 检查该 key 是否仍然可用（active 且未冷却）
			if key, err := p.getKeyIfAvailable(lastKeyID, groupID, cfg.CooldownDurationSeconds); err == nil {
				return key, nil
			}
		}
	}

	// 2. 按优先级和策略选择 key
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)

	// Atomically rotate the key ID from the list
	keyIDStr, err := p.store.Rotate(activeKeysListKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, app_errors.ErrNoActiveKeys
		}
		return nil, fmt.Errorf("failed to rotate key from store: %w", err)
	}

	keyID, err := strconv.ParseUint(keyIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key ID '%s': %w", keyIDStr, err)
	}

	// 3. 检查 key 是否在冷却中
	if p.isKeyCoolingDown(uint(keyID)) {
		// 如果在冷却中，递归尝试下一个 key（最多尝试 10 次避免无限循环）
		return p.selectNextAvailableKey(groupID, group, clientIdentifier, 0)
	}

	// 4. Get key details from HASH
	keyHashKey := fmt.Sprintf("key:%d", keyID)
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return nil, fmt.Errorf("failed to get key details for key ID %d: %w", keyID, err)
	}

	// 5. Manually unmarshal the map into an APIKey struct
	failureCount, _ := strconv.ParseInt(keyDetails["failure_count"], 10, 64)
	createdAt, _ := strconv.ParseInt(keyDetails["created_at"], 10, 64)
	priority, _ := strconv.Atoi(keyDetails["priority"])
	isManuallyDisabled := keyDetails["is_manually_disabled"] == "true"

	// 6. 如果 key 被手动禁用，跳过
	if isManuallyDisabled {
		return p.selectNextAvailableKey(groupID, group, clientIdentifier, 0)
	}

	// Decrypt the key value for use by channels
	encryptedKeyValue := keyDetails["key_string"]
	decryptedKeyValue, err := p.encryptionSvc.Decrypt(encryptedKeyValue)
	if err != nil {
		// If decryption fails, try to use the value as-is (backward compatibility for unencrypted keys)
		logrus.WithFields(logrus.Fields{
			"keyID": keyID,
			"error": err,
		}).Debug("Failed to decrypt key value, using as-is for backward compatibility")
		decryptedKeyValue = encryptedKeyValue
	}

	apiKey := &models.APIKey{
		ID:                 uint(keyID),
		KeyValue:           decryptedKeyValue,
		Status:             keyDetails["status"],
		Priority:           priority,
		IsManuallyDisabled: isManuallyDisabled,
		FailureCount:       failureCount,
		GroupID:            groupID,
		CreatedAt:          time.Unix(createdAt, 0),
	}

	// 7. 如果是 sticky 策略，记录本次使用的 key
	if strategy == "sticky" && clientIdentifier != "" {
		p.setLastUsedKey(groupID, clientIdentifier, uint(keyID))
	}

	return apiKey, nil
}

// UpdateStatus 异步地提交一个 Key 状态更新任务。
func (p *KeyProvider) UpdateStatus(apiKey *models.APIKey, group *models.Group, isSuccess bool, decision *app_errors.KeyFailureDecision) {
	go func() {
		keyHashKey := fmt.Sprintf("key:%d", apiKey.ID)
		activeKeysListKey := fmt.Sprintf("group:%d:active_keys", group.ID)

		if isSuccess {
			if err := p.handleSuccess(apiKey.ID, keyHashKey, activeKeysListKey); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle key success")
			}
		} else {
			if decision == nil {
				decision = &app_errors.KeyFailureDecision{
					Action:       models.KeyActionNormalFailure,
					StatusCode:   0,
					ErrorMessage: "",
					Retryable:    true,
				}
			}

			switch decision.Action {
			case models.KeyActionAutoDisable:
				logrus.WithFields(logrus.Fields{
					"keyID":      apiKey.ID,
					"statusCode": decision.StatusCode,
					"error":      decision.ErrorMessage,
				}).Warn("Auto-disable error detected, permanently disabling key")
				if err := p.permanentlyDisableKey(apiKey.ID, keyHashKey, activeKeysListKey, decision); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to permanently disable key")
				}
				return
			case models.KeyActionCooldown:
				cooldownSeconds := group.EffectiveConfig.CooldownDurationSeconds
				if cooldownSeconds > 0 {
					logrus.WithFields(logrus.Fields{
						"keyID":           apiKey.ID,
						"cooldownSeconds": cooldownSeconds,
						"statusCode":      decision.StatusCode,
						"errorMessage":    decision.ErrorMessage,
					}).Info("Cooldown error detected, setting key cooldown")
					if err := p.SetKeyCooldown(apiKey.ID, keyHashKey, cooldownSeconds, decision); err != nil {
						logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to set key cooldown")
					}
					return
				}
				fallbackDecision := *decision
				fallbackDecision.Action = models.KeyActionNormalFailure
				decision = &fallbackDecision
			case models.KeyActionDirectFail:
				if err := p.recordKeyObservation(apiKey.ID, keyHashKey, decision); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to record direct-fail observation")
				}
				return
			}

			// 正常失败处理
			if err := p.handleFailure(apiKey, group, keyHashKey, activeKeysListKey, decision); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "error": err}).Error("Failed to handle key failure")
			}
		}
	}()
}

// executeTransactionWithRetry wraps a database transaction with a retry mechanism.
func (p *KeyProvider) executeTransactionWithRetry(operation func(tx *gorm.DB) error) error {
	const maxRetries = 3
	const baseDelay = 50 * time.Millisecond
	const maxJitter = 150 * time.Millisecond
	var err error

	for i := range maxRetries {
		err = p.db.Transaction(operation)
		if err == nil {
			return nil
		}

		if strings.Contains(err.Error(), "database is locked") {
			jitter := time.Duration(rand.Intn(int(maxJitter)))
			totalDelay := baseDelay + jitter
			logrus.Debugf("Database is locked, retrying in %v... (attempt %d/%d)", totalDelay, i+1, maxRetries)
			time.Sleep(totalDelay)
			continue
		}

		break
	}

	return err
}

func (p *KeyProvider) handleSuccess(keyID uint, keyHashKey, activeKeysListKey string) error {
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return fmt.Errorf("failed to get key details from store: %w", err)
	}

	failureCount, _ := strconv.ParseInt(keyDetails["failure_count"], 10, 64)
	isActive := keyDetails["status"] == models.KeyStatusActive

	if failureCount == 0 && isActive {
		return nil
	}

	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", keyID, err)
		}

		updates := map[string]any{
			"failure_count":      0,
			"cooldown_until":     nil,
			"last_error_code":    0,
			"last_error_message": "",
			"last_status_action": models.KeyActionNone,
		}
		if !isActive {
			updates["status"] = models.KeyStatusActive
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key in DB: %w", err)
		}

		if err := p.store.HSet(keyHashKey, map[string]any{
			"failure_count":      0,
			"cooldown_until":     0,
			"last_error_code":    0,
			"last_error_message": "",
			"last_status_action": models.KeyActionNone,
		}); err != nil {
			return fmt.Errorf("failed to update key details in store: %w", err)
		}
		if err := p.store.Delete(fmt.Sprintf("cooldown:%d", keyID)); err != nil {
			return fmt.Errorf("failed to clear cooldown key: %w", err)
		}

		if !isActive {
			logrus.WithField("keyID", keyID).Debug("Key has recovered and is being restored to active pool.")
			if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
				return fmt.Errorf("failed to LRem key before LPush on recovery: %w", err)
			}
			if err := p.store.LPush(activeKeysListKey, keyID); err != nil {
				return fmt.Errorf("failed to LPush key back to active list: %w", err)
			}
		}

		return nil
	})
}

// permanentlyDisableKey 永久禁用 key（用于 402 等支付错误）
func (p *KeyProvider) permanentlyDisableKey(keyID uint, keyHashKey, activeKeysListKey string, decision *app_errors.KeyFailureDecision) error {
	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", keyID, err)
		}

		// 设置为 invalid 状态，不再自动恢复
		updates := map[string]any{
			"status":             models.KeyStatusInvalid,
			"failure_count":      key.FailureCount + 1,
			"cooldown_until":     nil,
			"last_error_code":    decision.StatusCode,
			"last_error_message": decision.ErrorMessage,
			"last_status_action": models.KeyActionAutoDisable,
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key in DB: %w", err)
		}

		// 从 active_keys 列表中移除
		if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
			return fmt.Errorf("failed to LRem key from active list: %w", err)
		}
		if err := p.store.Delete(fmt.Sprintf("cooldown:%d", keyID)); err != nil {
			return fmt.Errorf("failed to clear cooldown key: %w", err)
		}

		// 更新缓存
		if err := p.store.HSet(keyHashKey, map[string]any{
			"status":             models.KeyStatusInvalid,
			"failure_count":      key.FailureCount + 1,
			"cooldown_until":     0,
			"last_error_code":    decision.StatusCode,
			"last_error_message": decision.ErrorMessage,
			"last_status_action": models.KeyActionAutoDisable,
		}); err != nil {
			return fmt.Errorf("failed to update key status in store: %w", err)
		}

		return nil
	})
}

func (p *KeyProvider) handleFailure(apiKey *models.APIKey, group *models.Group, keyHashKey, activeKeysListKey string, decision *app_errors.KeyFailureDecision) error {
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return fmt.Errorf("failed to get key details from store: %w", err)
	}

	if keyDetails["status"] == models.KeyStatusInvalid {
		return nil
	}

	failureCount, _ := strconv.ParseInt(keyDetails["failure_count"], 10, 64)

	// 获取该分组的有效配置
	blacklistThreshold := group.EffectiveConfig.BlacklistThreshold

	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, apiKey.ID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", apiKey.ID, err)
		}

		newFailureCount := failureCount + 1
		action := models.KeyActionNormalFailure
		statusCode := 0
		errorMessage := ""
		if decision != nil {
			if decision.Action != "" {
				action = decision.Action
			}
			statusCode = decision.StatusCode
			errorMessage = decision.ErrorMessage
		}

		updates := map[string]any{
			"failure_count":      newFailureCount,
			"cooldown_until":     nil,
			"last_error_code":    statusCode,
			"last_error_message": errorMessage,
			"last_status_action": action,
		}
		shouldBlacklist := blacklistThreshold > 0 && newFailureCount >= int64(blacklistThreshold)
		if shouldBlacklist {
			updates["status"] = models.KeyStatusInvalid
			updates["last_status_action"] = models.KeyActionBlacklisted
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key stats in DB: %w", err)
		}

		if _, err := p.store.HIncrBy(keyHashKey, "failure_count", 1); err != nil {
			return fmt.Errorf("failed to increment failure count in store: %w", err)
		}
		if err := p.store.HSet(keyHashKey, map[string]any{
			"cooldown_until":     0,
			"last_error_code":    statusCode,
			"last_error_message": errorMessage,
			"last_status_action": updates["last_status_action"],
		}); err != nil {
			return fmt.Errorf("failed to update key metadata in store: %w", err)
		}
		if err := p.store.Delete(fmt.Sprintf("cooldown:%d", apiKey.ID)); err != nil {
			return fmt.Errorf("failed to clear cooldown key: %w", err)
		}

		if shouldBlacklist {
			logrus.WithFields(logrus.Fields{"keyID": apiKey.ID, "threshold": blacklistThreshold}).Warn("Key has reached blacklist threshold, disabling.")
			if err := p.store.LRem(activeKeysListKey, 0, apiKey.ID); err != nil {
				return fmt.Errorf("failed to LRem key from active list: %w", err)
			}
			if err := p.store.HSet(keyHashKey, map[string]any{
				"status":             models.KeyStatusInvalid,
				"last_status_action": models.KeyActionBlacklisted,
			}); err != nil {
				return fmt.Errorf("failed to update key status to invalid in store: %w", err)
			}
		}

		return nil
	})
}

func (p *KeyProvider) recordKeyObservation(keyID uint, keyHashKey string, decision *app_errors.KeyFailureDecision) error {
	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d for update: %w", keyID, err)
		}

		updates := map[string]any{
			"last_error_code":    decision.StatusCode,
			"last_error_message": decision.ErrorMessage,
			"last_status_action": models.KeyActionDirectFail,
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key observation in DB: %w", err)
		}
		if err := p.store.HSet(keyHashKey, updates); err != nil {
			return fmt.Errorf("failed to update key observation in store: %w", err)
		}

		return nil
	})
}

// LoadKeysFromDB 从数据库加载所有分组和密钥，并填充到 Store 中。
func (p *KeyProvider) LoadKeysFromDB() error {
	logrus.Debug("First time startup, loading keys from DB...")

	// 1. 分批从数据库加载并使用 Pipeline 写入 Redis
	allActiveKeyIDs := make(map[uint][]any)
	batchSize := 10000
	var batchKeys []*models.APIKey

	err := p.db.Model(&models.APIKey{}).FindInBatches(&batchKeys, batchSize, func(tx *gorm.DB, batch int) error {
		logrus.Debugf("Processing batch %d with %d keys...", batch, len(batchKeys))

		var pipeline store.Pipeliner
		if redisStore, ok := p.store.(store.RedisPipeliner); ok {
			pipeline = redisStore.Pipeline()
		}

		for _, key := range batchKeys {
			keyHashKey := fmt.Sprintf("key:%d", key.ID)
			keyDetails := p.apiKeyToMap(key)

			if pipeline != nil {
				pipeline.HSet(keyHashKey, keyDetails)
			} else {
				if err := p.store.HSet(keyHashKey, keyDetails); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to HSet key details")
				}
			}

			if key.Status == models.KeyStatusActive {
				allActiveKeyIDs[key.GroupID] = append(allActiveKeyIDs[key.GroupID], key.ID)
			}

			if key.CooldownUntil != nil && key.CooldownUntil.After(time.Now()) {
				cooldownKey := fmt.Sprintf("cooldown:%d", key.ID)
				ttl := time.Until(*key.CooldownUntil)
				if err := p.store.Set(cooldownKey, []byte(strconv.FormatInt(key.CooldownUntil.Unix(), 10)), ttl); err != nil {
					logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to restore cooldown state")
				}
			}
		}

		if pipeline != nil {
			if err := pipeline.Exec(); err != nil {
				return fmt.Errorf("failed to execute pipeline for batch %d: %w", batch, err)
			}
		}
		return nil
	}).Error

	if err != nil {
		return fmt.Errorf("failed during batch processing of keys: %w", err)
	}

	// 2. 更新所有分组的 active_keys 列表
	logrus.Info("Updating active key lists for all groups...")
	for groupID, activeIDs := range allActiveKeyIDs {
		if len(activeIDs) > 0 {
			activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
			p.store.Delete(activeKeysListKey)
			if err := p.store.LPush(activeKeysListKey, activeIDs...); err != nil {
				logrus.WithFields(logrus.Fields{"groupID": groupID, "error": err}).Error("Failed to LPush active keys for group")
			}
		}
	}

	return nil
}

// AddKeys 批量添加新的 Key 到池和数据库中。
func (p *KeyProvider) AddKeys(groupID uint, keys []models.APIKey) error {
	if len(keys) == 0 {
		return nil
	}

	err := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&keys).Error; err != nil {
			return err
		}

		// 使用批量方法添加到缓存
		return p.addKeysToCacheBatch(groupID, keys)
	})

	return err
}

// RemoveKeys 批量从池和数据库中移除 Key。
func (p *KeyProvider) RemoveKeys(groupID uint, keyValues []string) (int64, error) {
	if len(keyValues) == 0 {
		return 0, nil
	}

	var keysToDelete []models.APIKey
	var deletedCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		var keyHashes []string
		for _, keyValue := range keyValues {
			keyHash := p.encryptionSvc.Hash(keyValue)
			if keyHash != "" {
				keyHashes = append(keyHashes, keyHash)
			}
		}

		if len(keyHashes) == 0 {
			return nil
		}

		if err := tx.Where("group_id = ? AND key_hash IN ?", groupID, keyHashes).Find(&keysToDelete).Error; err != nil {
			return err
		}

		if len(keysToDelete) == 0 {
			return nil
		}

		keyIDsToDelete := pluckIDs(keysToDelete)

		result := tx.Where("id IN ?", keyIDsToDelete).Delete(&models.APIKey{})
		if result.Error != nil {
			return result.Error
		}
		deletedCount = result.RowsAffected

		for _, key := range keysToDelete {
			if err := p.removeKeyFromStore(key.ID, key.GroupID); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to remove key from store after DB deletion, rolling back transaction")
				return err
			}
		}

		return nil
	})

	return deletedCount, err
}

// RestoreKeys 恢复组内所有无效的 Key。
func (p *KeyProvider) RestoreKeys(groupID uint) (int64, error) {
	var invalidKeys []models.APIKey
	var restoredCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("group_id = ? AND status = ?", groupID, models.KeyStatusInvalid).Find(&invalidKeys).Error; err != nil {
			return err
		}

		if len(invalidKeys) == 0 {
			return nil
		}

		updates := map[string]any{
			"status":             models.KeyStatusActive,
			"failure_count":      0,
			"cooldown_until":     nil,
			"last_error_code":    0,
			"last_error_message": "",
			"last_status_action": models.KeyActionNone,
		}
		result := tx.Model(&models.APIKey{}).Where("group_id = ? AND status = ?", groupID, models.KeyStatusInvalid).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		restoredCount = result.RowsAffected

		for _, key := range invalidKeys {
			key.Status = models.KeyStatusActive
			key.FailureCount = 0
			key.CooldownUntil = nil
			key.LastErrorCode = 0
			key.LastErrorMessage = ""
			key.LastStatusAction = models.KeyActionNone
			if err := p.addKeyToStore(&key); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to restore key in store after DB update, rolling back transaction")
				return err
			}
		}
		return nil
	})

	return restoredCount, err
}

// RestoreMultipleKeys 恢复指定的 Key。
func (p *KeyProvider) RestoreMultipleKeys(groupID uint, keyValues []string) (int64, error) {
	if len(keyValues) == 0 {
		return 0, nil
	}

	var keysToRestore []models.APIKey
	var restoredCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		var keyHashes []string
		for _, keyValue := range keyValues {
			keyHash := p.encryptionSvc.Hash(keyValue)
			if keyHash != "" {
				keyHashes = append(keyHashes, keyHash)
			}
		}

		if len(keyHashes) == 0 {
			return nil
		}

		if err := tx.Where("group_id = ? AND key_hash IN ? AND status = ?", groupID, keyHashes, models.KeyStatusInvalid).Find(&keysToRestore).Error; err != nil {
			return err
		}

		if len(keysToRestore) == 0 {
			return nil
		}

		keyIDsToRestore := pluckIDs(keysToRestore)

		updates := map[string]any{
			"status":             models.KeyStatusActive,
			"failure_count":      0,
			"cooldown_until":     nil,
			"last_error_code":    0,
			"last_error_message": "",
			"last_status_action": models.KeyActionNone,
		}
		result := tx.Model(&models.APIKey{}).Where("id IN ?", keyIDsToRestore).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		restoredCount = result.RowsAffected

		for _, key := range keysToRestore {
			key.Status = models.KeyStatusActive
			key.FailureCount = 0
			key.CooldownUntil = nil
			key.LastErrorCode = 0
			key.LastErrorMessage = ""
			key.LastStatusAction = models.KeyActionNone
			if err := p.addKeyToStore(&key); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to restore key in store after DB update")
				return err
			}
		}

		return nil
	})

	return restoredCount, err
}

// RemoveInvalidKeys 移除组内所有无效的 Key。
func (p *KeyProvider) RemoveInvalidKeys(groupID uint) (int64, error) {
	return p.removeKeysByStatus(groupID, models.KeyStatusInvalid)
}

// RemoveAllKeys 移除组内所有的 Key。
func (p *KeyProvider) RemoveAllKeys(groupID uint) (int64, error) {
	return p.removeKeysByStatus(groupID)
}

// removeKeysByStatus is a generic function to remove keys by status.
// If no status is provided, it removes all keys in the group.
func (p *KeyProvider) removeKeysByStatus(groupID uint, status ...string) (int64, error) {
	var keysToRemove []models.APIKey
	var removedCount int64

	err := p.db.Transaction(func(tx *gorm.DB) error {
		query := tx.Where("group_id = ?", groupID)
		if len(status) > 0 {
			query = query.Where("status IN ?", status)
		}

		if err := query.Find(&keysToRemove).Error; err != nil {
			return err
		}

		if len(keysToRemove) == 0 {
			return nil
		}

		deleteQuery := tx.Where("group_id = ?", groupID)
		if len(status) > 0 {
			deleteQuery = deleteQuery.Where("status IN ?", status)
		}
		result := deleteQuery.Delete(&models.APIKey{})
		if result.Error != nil {
			return result.Error
		}
		removedCount = result.RowsAffected

		for _, key := range keysToRemove {
			if err := p.removeKeyFromStore(key.ID, key.GroupID); err != nil {
				logrus.WithFields(logrus.Fields{"keyID": key.ID, "error": err}).Error("Failed to remove key from store after DB deletion, rolling back transaction")
				return err
			}
		}
		return nil
	})

	return removedCount, err
}

// RemoveKeysFromStore 直接从内存存储中移除指定的键，不涉及数据库操作
// 这个方法适用于数据库已经删除但需要清理内存存储的场景
func (p *KeyProvider) RemoveKeysFromStore(groupID uint, keyIDs []uint) error {
	if len(keyIDs) == 0 {
		return nil
	}

	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)

	// 第一步：直接删除整个 active_keys 列表
	if err := p.store.Delete(activeKeysListKey); err != nil {
		logrus.WithFields(logrus.Fields{
			"groupID": groupID,
			"error":   err,
		}).Error("Failed to delete active keys list")
		return err
	}

	// 第二步：批量删除所有相关的key hash
	for _, keyID := range keyIDs {
		cooldownKey := fmt.Sprintf("cooldown:%d", keyID)
		if err := p.store.Delete(cooldownKey); err != nil {
			logrus.WithFields(logrus.Fields{
				"keyID": keyID,
				"error": err,
			}).Error("Failed to delete cooldown key")
		}

		keyHashKey := fmt.Sprintf("key:%d", keyID)
		if err := p.store.Delete(keyHashKey); err != nil {
			logrus.WithFields(logrus.Fields{
				"keyID": keyID,
				"error": err,
			}).Error("Failed to delete key hash")
		}
	}

	logrus.WithFields(logrus.Fields{
		"groupID":  groupID,
		"keyCount": len(keyIDs),
	}).Info("Successfully cleaned up group keys from store")

	return nil
}

// addKeyToStore is a helper to add a single key to the cache.
func (p *KeyProvider) addKeyToStore(key *models.APIKey) error {
	// 1. Store key details in HASH
	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	keyDetails := p.apiKeyToMap(key)
	if err := p.store.HSet(keyHashKey, keyDetails); err != nil {
		return fmt.Errorf("failed to HSet key details for key %d: %w", key.ID, err)
	}

	// 2. If active, add to the active LIST
	if key.Status == models.KeyStatusActive {
		activeKeysListKey := fmt.Sprintf("group:%d:active_keys", key.GroupID)
		if err := p.store.LRem(activeKeysListKey, 0, key.ID); err != nil {
			return fmt.Errorf("failed to LRem key %d before LPush for group %d: %w", key.ID, key.GroupID, err)
		}
		if err := p.store.LPush(activeKeysListKey, key.ID); err != nil {
			return fmt.Errorf("failed to LPush key %d to group %d: %w", key.ID, key.GroupID, err)
		}
	}

	cooldownKey := fmt.Sprintf("cooldown:%d", key.ID)
	if key.CooldownUntil != nil && key.CooldownUntil.After(time.Now()) {
		if err := p.store.Set(cooldownKey, []byte(strconv.FormatInt(key.CooldownUntil.Unix(), 10)), time.Until(*key.CooldownUntil)); err != nil {
			return fmt.Errorf("failed to restore cooldown for key %d: %w", key.ID, err)
		}
	} else if err := p.store.Delete(cooldownKey); err != nil {
		return fmt.Errorf("failed to clear cooldown for key %d: %w", key.ID, err)
	}

	return nil
}

// addKeysToCacheBatch 批量添加密钥到缓存（用于批量导入场景）
func (p *KeyProvider) addKeysToCacheBatch(groupID uint, keys []models.APIKey) error {
	if len(keys) == 0 {
		return nil
	}

	// 1. 批量 HSet 密钥详情
	if pipeliner, ok := p.store.(store.RedisPipeliner); ok {
		// Redis: 使用 Pipeline 批量操作
		pipe := pipeliner.Pipeline()
		for i := range keys {
			keyHashKey := fmt.Sprintf("key:%d", keys[i].ID)
			pipe.HSet(keyHashKey, p.apiKeyToMap(&keys[i]))
		}
		if err := pipe.Exec(); err != nil {
			return fmt.Errorf("failed to batch HSet keys: %w", err)
		}
	} else {
		// MemoryStore: 降级为逐个 HSet
		for i := range keys {
			keyHashKey := fmt.Sprintf("key:%d", keys[i].ID)
			if err := p.store.HSet(keyHashKey, p.apiKeyToMap(&keys[i])); err != nil {
				return fmt.Errorf("failed to HSet key %d: %w", keys[i].ID, err)
			}
		}
	}

	// 2. 收集所有密钥 ID
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
	activeKeyIDs := make([]any, len(keys))
	for i := range keys {
		activeKeyIDs[i] = keys[i].ID
	}

	// 3. 批量 LPush 活跃密钥
	if err := p.store.LPush(activeKeysListKey, activeKeyIDs...); err != nil {
		return fmt.Errorf("failed to batch LPush keys to group %d: %w", groupID, err)
	}

	return nil
}

// removeKeyFromStore is a helper to remove a single key from the cache.
func (p *KeyProvider) removeKeyFromStore(keyID, groupID uint) error {
	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
	if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
		logrus.WithFields(logrus.Fields{"keyID": keyID, "groupID": groupID, "error": err}).Error("Failed to LRem key from active list")
	}

	cooldownKey := fmt.Sprintf("cooldown:%d", keyID)
	if err := p.store.Delete(cooldownKey); err != nil {
		return fmt.Errorf("failed to delete cooldown key for key %d: %w", keyID, err)
	}

	keyHashKey := fmt.Sprintf("key:%d", keyID)
	if err := p.store.Delete(keyHashKey); err != nil {
		return fmt.Errorf("failed to delete key HASH for key %d: %w", keyID, err)
	}
	return nil
}

// apiKeyToMap converts an APIKey model to a map for HSET.
func (p *KeyProvider) apiKeyToMap(key *models.APIKey) map[string]any {
	return map[string]any{
		"id":                   fmt.Sprint(key.ID),
		"key_string":           key.KeyValue,
		"status":               key.Status,
		"priority":             key.Priority,
		"is_manually_disabled": key.IsManuallyDisabled,
		"failure_count":        key.FailureCount,
		"cooldown_until":       cooldownUnix(key.CooldownUntil),
		"last_error_code":      key.LastErrorCode,
		"last_error_message":   key.LastErrorMessage,
		"last_status_action":   key.LastStatusAction,
		"group_id":             key.GroupID,
		"created_at":           key.CreatedAt.Unix(),
	}
}

// pluckIDs extracts IDs from a slice of APIKey.
func pluckIDs(keys []models.APIKey) []uint {
	ids := make([]uint, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	return ids
}

// getLastUsedKey 获取客户端上次成功使用的 key ID
func (p *KeyProvider) getLastUsedKey(groupID uint, clientIdentifier string) (uint, error) {
	key := fmt.Sprintf("sticky:%d:%s", groupID, clientIdentifier)
	data, err := p.store.Get(key)
	if err != nil {
		return 0, err
	}
	keyID, err := strconv.ParseUint(string(data), 10, 64)
	if err != nil {
		return 0, err
	}
	return uint(keyID), nil
}

// setLastUsedKey 记录客户端本次成功使用的 key ID
func (p *KeyProvider) setLastUsedKey(groupID uint, clientIdentifier string, keyID uint) {
	key := fmt.Sprintf("sticky:%d:%s", groupID, clientIdentifier)
	// 设置 24 小时过期
	p.store.Set(key, []byte(fmt.Sprint(keyID)), 24*time.Hour)
}

// getKeyIfAvailable 检查指定 key 是否可用（active 且未冷却）
func (p *KeyProvider) getKeyIfAvailable(keyID uint, groupID uint, cooldownSeconds int) (*models.APIKey, error) {
	// 检查是否在冷却中
	if p.isKeyCoolingDown(keyID) {
		return nil, fmt.Errorf("key is cooling down")
	}

	keyHashKey := fmt.Sprintf("key:%d", keyID)
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return nil, err
	}

	// 检查状态
	if keyDetails["status"] != models.KeyStatusActive {
		return nil, fmt.Errorf("key is not active")
	}

	// 检查是否手动禁用
	if keyDetails["is_manually_disabled"] == "true" {
		return nil, fmt.Errorf("key is manually disabled")
	}

	// 解密并返回
	failureCount, _ := strconv.ParseInt(keyDetails["failure_count"], 10, 64)
	createdAt, _ := strconv.ParseInt(keyDetails["created_at"], 10, 64)
	priority, _ := strconv.Atoi(keyDetails["priority"])

	encryptedKeyValue := keyDetails["key_string"]
	decryptedKeyValue, err := p.encryptionSvc.Decrypt(encryptedKeyValue)
	if err != nil {
		decryptedKeyValue = encryptedKeyValue
	}

	return &models.APIKey{
		ID:                 keyID,
		KeyValue:           decryptedKeyValue,
		Status:             keyDetails["status"],
		Priority:           priority,
		IsManuallyDisabled: false,
		FailureCount:       failureCount,
		GroupID:            groupID,
		CreatedAt:          time.Unix(createdAt, 0),
	}, nil
}

// selectNextAvailableKey 递归选择下一个可用的 key（避免冷却中的 key）
func (p *KeyProvider) selectNextAvailableKey(groupID uint, group *models.Group, clientIdentifier string, attempt int) (*models.APIKey, error) {
	const maxAttempts = 10
	if attempt >= maxAttempts {
		return nil, app_errors.ErrNoActiveKeys
	}

	activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
	keyIDStr, err := p.store.Rotate(activeKeysListKey)
	if err != nil {
		return nil, app_errors.ErrNoActiveKeys
	}

	keyID, err := strconv.ParseUint(keyIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key ID: %w", err)
	}

	// 检查是否在冷却中或手动禁用
	if p.isKeyCoolingDown(uint(keyID)) {
		return p.selectNextAvailableKey(groupID, group, clientIdentifier, attempt+1)
	}

	keyHashKey := fmt.Sprintf("key:%d", keyID)
	keyDetails, err := p.store.HGetAll(keyHashKey)
	if err != nil {
		return p.selectNextAvailableKey(groupID, group, clientIdentifier, attempt+1)
	}

	if keyDetails["is_manually_disabled"] == "true" {
		return p.selectNextAvailableKey(groupID, group, clientIdentifier, attempt+1)
	}

	// 构造返回的 key
	failureCount, _ := strconv.ParseInt(keyDetails["failure_count"], 10, 64)
	createdAt, _ := strconv.ParseInt(keyDetails["created_at"], 10, 64)
	priority, _ := strconv.Atoi(keyDetails["priority"])

	encryptedKeyValue := keyDetails["key_string"]
	decryptedKeyValue, err := p.encryptionSvc.Decrypt(encryptedKeyValue)
	if err != nil {
		decryptedKeyValue = encryptedKeyValue
	}

	return &models.APIKey{
		ID:                 uint(keyID),
		KeyValue:           decryptedKeyValue,
		Status:             keyDetails["status"],
		Priority:           priority,
		IsManuallyDisabled: false,
		FailureCount:       failureCount,
		GroupID:            groupID,
		CreatedAt:          time.Unix(createdAt, 0),
	}, nil
}

// isKeyCoolingDown 检查 key 是否在冷却中
func (p *KeyProvider) isKeyCoolingDown(keyID uint) bool {
	cooldownKey := fmt.Sprintf("cooldown:%d", keyID)
	exists, err := p.store.Exists(cooldownKey)
	if err != nil {
		return false
	}
	return exists
}

// SetKeyCooldown 将 key 设置为冷却状态
func (p *KeyProvider) SetKeyCooldown(keyID uint, keyHashKey string, durationSeconds int, decision *app_errors.KeyFailureDecision) error {
	if durationSeconds <= 0 {
		return nil
	}

	cooldownUntil := time.Now().Add(time.Duration(durationSeconds) * time.Second)
	updates := map[string]any{
		"cooldown_until":     cooldownUntil,
		"last_error_code":    decision.StatusCode,
		"last_error_message": decision.ErrorMessage,
		"last_status_action": models.KeyActionCooldown,
	}
	cacheUpdates := map[string]any{
		"cooldown_until":     cooldownUntil.Unix(),
		"last_error_code":    decision.StatusCode,
		"last_error_message": decision.ErrorMessage,
		"last_status_action": models.KeyActionCooldown,
	}

	if err := p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d: %w", keyID, err)
		}

		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update cooldown metadata in DB: %w", err)
		}
		if err := p.store.HSet(keyHashKey, cacheUpdates); err != nil {
			return fmt.Errorf("failed to update cooldown metadata in store: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	cooldownKey := fmt.Sprintf("cooldown:%d", keyID)
	return p.store.Set(cooldownKey, []byte(strconv.FormatInt(cooldownUntil.Unix(), 10)), time.Duration(durationSeconds)*time.Second)
}

// ClearKeyCooldown 手动清除 key 的冷却状态
func (p *KeyProvider) ClearKeyCooldown(keyID uint, groupID uint) error {
	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d: %w", keyID, err)
		}

		updates := map[string]any{
			"cooldown_until":     nil,
			"last_status_action": models.KeyActionNone,
		}
		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to clear cooldown in DB: %w", err)
		}

		keyHashKey := fmt.Sprintf("key:%d", keyID)
		if err := p.store.HSet(keyHashKey, map[string]any{
			"cooldown_until":     0,
			"last_status_action": models.KeyActionNone,
		}); err != nil {
			return fmt.Errorf("failed to clear cooldown in store: %w", err)
		}

		cooldownKey := fmt.Sprintf("cooldown:%d", keyID)
		if err := p.store.Delete(cooldownKey); err != nil {
			return fmt.Errorf("failed to delete cooldown key: %w", err)
		}

		// 如果 key 状态为 active 且未被手动禁用，确保在 active_keys 列表中
		if key.Status == models.KeyStatusActive && !key.IsManuallyDisabled {
			activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
			if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
				return fmt.Errorf("failed to LRem before LPush: %w", err)
			}
			if err := p.store.LPush(activeKeysListKey, keyID); err != nil {
				return fmt.Errorf("failed to add key to active list: %w", err)
			}
		}

		return nil
	})
}

// SetKeyManuallyDisabled 手动启用/禁用 key
func (p *KeyProvider) SetKeyManuallyDisabled(keyID uint, groupID uint, disabled bool) error {
	return p.executeTransactionWithRetry(func(tx *gorm.DB) error {
		var key models.APIKey
		if err := tx.Set("gorm:query_option", "FOR UPDATE").First(&key, keyID).Error; err != nil {
			return fmt.Errorf("failed to lock key %d: %w", keyID, err)
		}

		updates := map[string]any{"is_manually_disabled": disabled}
		if err := tx.Model(&key).Updates(updates).Error; err != nil {
			return fmt.Errorf("failed to update key in DB: %w", err)
		}

		keyHashKey := fmt.Sprintf("key:%d", keyID)
		if err := p.store.HSet(keyHashKey, updates); err != nil {
			return fmt.Errorf("failed to update key in store: %w", err)
		}

		// 如果是禁用，从 active_keys 列表中移除
		activeKeysListKey := fmt.Sprintf("group:%d:active_keys", groupID)
		if disabled {
			if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
				return fmt.Errorf("failed to remove key from active list: %w", err)
			}
		} else {
			// 如果是启用且状态为 active，加入 active_keys 列表
			if key.Status == models.KeyStatusActive {
				if err := p.store.LRem(activeKeysListKey, 0, keyID); err != nil {
					return fmt.Errorf("failed to LRem before LPush: %w", err)
				}
				if err := p.store.LPush(activeKeysListKey, keyID); err != nil {
					return fmt.Errorf("failed to add key to active list: %w", err)
				}
			}
		}

		return nil
	})
}

func cooldownUnix(ts *time.Time) int64 {
	if ts == nil {
		return 0
	}
	return ts.Unix()
}
